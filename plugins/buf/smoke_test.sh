#!/usr/bin/env bash
# Real smoke for the buf plugin's IMAGE. Runs the PINNED binary end to end against
# throwaway proto workspaces so a base bump that changes buf's CLI surface, exit
# codes, or config schema fails HERE, before publish. Proves the bump
# (bufbuild/buf 1.47 -> 1.75.0, alpine 3.24.4) across the three subcommands the
# plugin advertises:
#   - the binary is v1.75.0; a missing command fails fast (exit 2);
#   - `buf lint` passes on a clean module (0) and fails on a violation (non-zero);
#   - `buf breaking --against .git#branch=main` detects a deleted field against a
#     disposable git repo — exercising git, the entrypoint's safe.directory, and
#     the word-split of a multi-arg command;
#   - `buf generate` runs a local (offline) plugin from buf.gen.yaml to completion.
# NB: this proves the wrapper CONTRACT, not that findings are identical to 1.47 —
# lint/breaking analyses evolved across the bump, so results may legitimately move.
#
# Fully offline. Convention-named so plugins.yml runs it as a PRE-PUBLISH gate.
# Needs docker (+ git on the host for the breaking fixture). Built --no-cache to
# match the no_cache publish build. Trap cleanup, fail-closed on leftovers.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-buf:smoke-$$"
WC="$(mktemp -d)"   # clean lint module
WV="$(mktemp -d)"   # module with a lint violation
WB="$(mktemp -d)"   # breaking: git repo
WG="$(mktemp -d)"   # generate: local plugin
SMOKE_OK=0
cleanup() {
  # buf may write root-owned output (e.g. gen/) into the mounts; remove from
  # inside the image (matching uid) before deleting the dirs, then verify.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$WC:/c" -v "$WV:/v" -v "$WB:/b" -v "$WG:/g" "$IMG" \
      -c 'for d in /c /v /b /g; do rm -rf "$d"/* "$d"/.[!.]* "$d"/..?* 2>/dev/null || true; done' \
      >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WC" "$WV" "$WB" "$WG" 2>/dev/null || true
  leftover=0
  for d in "$WC" "$WV" "$WB" "$WG"; do
    [ -d "$d" ] && { echo "WARN: cleanup could not fully remove ${d}" >&2; leftover=1; }
  done
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    echo "WARN: smoke image ${IMG} was not removed" >&2; leftover=1
  fi
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

cfg() {  # cfg <dir> — write a v2 buf.yaml
  cat >"$1/buf.yaml" <<'YAML'
version: v2
modules:
  - path: proto
lint:
  use:
    - STANDARD
YAML
}
proto() { # proto <dir> <field-decl>  (field-decl e.g. "string thing_id = 1;")
  mkdir -p "$1/proto/acme/thing/v1"
  { echo 'syntax = "proto3";'; echo 'package acme.thing.v1;'; echo 'message Thing {'; echo "  $2"; echo '}'; } \
    >"$1/proto/acme/thing/v1/thing.proto"
}

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> buf is v1.75.0"
OUT="$(docker run --rm --network none --entrypoint buf "$IMG" --version 2>&1)" || die "buf --version failed" "$OUT"
grep -q '1.75.0' <<<"$OUT" || die "buf is not v1.75.0 (got: ${OUT})" "$OUT"

echo "==> missing command fails fast (exit 2)"
set +e
OUT="$(docker run --rm --network none "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 2 ] || die "missing-command: expected exit 2, got $RC" "$OUT"
grep -q 'PLUGIN_COMMAND is required' <<<"$OUT" || die "missing-command: wrong message" "$OUT"

echo "==> lint CLEAN passes (exit 0)"
cfg "$WC"; proto "$WC" 'string thing_id = 1;'
set +e
OUT="$(docker run --rm --network none -v "$WC:/workspace" -w /workspace -e PLUGIN_COMMAND="lint" "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "lint-clean: expected exit 0, got $RC" "$OUT"

echo "==> lint VIOLATION fails (non-zero) and names the rule"
cfg "$WV"; proto "$WV" 'string ThingID = 1;'   # UpperCamel violates FIELD_LOWER_SNAKE_CASE
set +e
OUT="$(docker run --rm --network none -v "$WV:/workspace" -w /workspace -e PLUGIN_COMMAND="lint" "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || die "lint-violation: expected a non-zero exit, got 0" "$OUT"
grep -qi 'lower_snake_case' <<<"$OUT" || die "lint-violation: buf did not report the expected lint error" "$OUT"

echo "==> breaking --against a disposable git baseline detects a deleted field"
cfg "$WB"; proto "$WB" 'string thing_id = 1;
  string name = 2;'
git -C "$WB" init -q -b main
git -C "$WB" -c user.email=smoke@example.invalid -c user.name=smoke add -A
git -C "$WB" -c user.email=smoke@example.invalid -c user.name=smoke commit -qm baseline
proto "$WB" 'string thing_id = 1;'   # dropped field 2 = breaking
set +e
OUT="$(docker run --rm --network none -v "$WB:/workspace" -w /workspace \
  -e PLUGIN_COMMAND="breaking --against .git#branch=main" "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || die "breaking: expected a non-zero exit for a deleted field, got 0" "$OUT"
grep -qiE 'was deleted|previously present' <<<"$OUT" || die "breaking: buf did not report the deleted field (git/safe.directory/word-split?)" "$OUT"

echo "==> generate runs a local (offline) plugin and WRITES its output"
cfg "$WG"; proto "$WG" 'string thing_id = 1;'
# A local protoc plugin that discards the CodeGeneratorRequest and emits a valid
# CodeGeneratorResponse carrying ONE file (gen-marker.txt = "buf-generate-ok"),
# hand-encoded as protobuf. buf must actually run it AND consume its output — so
# the generated file appearing under gen/ proves the whole pipeline, offline. A
# noop plugin (exit 0, no output) would pass even if buf ignored the plugins
# entry; a written, correct file cannot.
cat >"$WG/emit-plugin" <<'SH'
#!/bin/sh
cat >/dev/null
name="gen-marker.txt"; content="buf-generate-ok"
# CodeGeneratorResponse.File{ name=field1, content=field15 }, wrapped in
# CodeGeneratorResponse.file=field15.
file=$(printf '\x0a%b%s\x7a%b%s' "$(printf '\\x%02x' ${#name})" "$name" "$(printf '\\x%02x' ${#content})" "$content")
printf '\x7a%b%s' "$(printf '\\x%02x' ${#file})" "$file"
SH
chmod +x "$WG/emit-plugin"
cat >"$WG/buf.gen.yaml" <<'YAML'
version: v2
plugins:
  - local: ./emit-plugin
    out: gen
YAML
set +e
OUT="$(docker run --rm --network none -v "$WG:/workspace" -w /workspace -e PLUGIN_COMMAND="generate" "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "generate: expected exit 0 with a local plugin, got $RC" "$OUT"
gen_content="$(docker run --rm --network none -v "$WG:/w" --entrypoint cat "$IMG" /w/gen/gen-marker.txt 2>/dev/null || true)"
[ "$gen_content" = "buf-generate-ok" ] \
  || die "generate: the local plugin's output file is missing/wrong (buf ignored the plugins entry?) — got '${gen_content}'" "$OUT"

SMOKE_OK=1
echo "PASS: buf smoke"
