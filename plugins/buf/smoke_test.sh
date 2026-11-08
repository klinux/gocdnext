#!/usr/bin/env bash
# Real smoke for the buf plugin's IMAGE. Runs the PINNED binary end to end against
# throwaway proto workspaces so a base bump that changes buf's CLI surface or exit
# codes fails HERE, before publish. Proves the bump (bufbuild/buf 1.47 -> 1.75.0,
# alpine 3.24.4):
#   - the binary is v1.75.0;
#   - a missing command fails fast (exit 2);
#   - `buf lint` on a clean module passes (exit 0);
#   - `buf lint` on a module with a lint violation fails (buf's non-zero exit)
#     and prints the offending rule.
#
# Fully offline (lint reads local protos only). Convention-named so plugins.yml
# runs it as a PRE-PUBLISH gate. Needs docker. Built --no-cache to match the
# no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-buf:smoke-$$"
WC="$(mktemp -d)"   # clean module
WV="$(mktemp -d)"   # module with a lint violation
SMOKE_OK=0
cleanup() {
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WC" "$WV" 2>/dev/null || true
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

mkmodule() { # mkmodule <dir> <field-name>
  mkdir -p "$1/proto/acme/thing/v1"
  cat >"$1/buf.yaml" <<'YAML'
version: v2
modules:
  - path: proto
lint:
  use:
    - STANDARD
YAML
  cat >"$1/proto/acme/thing/v1/thing.proto" <<PROTO
syntax = "proto3";
package acme.thing.v1;
message Thing {
  string $2 = 1;
}
PROTO
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

echo "==> lint CLEAN module passes (exit 0)"
mkmodule "$WC" thing_id
set +e
OUT="$(docker run --rm --network none -v "$WC:/workspace" -w /workspace -e PLUGIN_COMMAND="lint" "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "lint-clean: expected exit 0, got $RC" "$OUT"

echo "==> lint VIOLATION fails (non-zero) and names the rule"
mkmodule "$WV" ThingID   # UpperCamel field violates FIELD_LOWER_SNAKE_CASE
set +e
OUT="$(docker run --rm --network none -v "$WV:/workspace" -w /workspace -e PLUGIN_COMMAND="lint" "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || die "lint-violation: expected a non-zero exit, got 0" "$OUT"
grep -qi 'lower_snake_case' <<<"$OUT" || die "lint-violation: buf did not report the expected lint error" "$OUT"

SMOKE_OK=1
echo "PASS: buf smoke"
