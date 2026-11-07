#!/usr/bin/env bash
# Real smoke for the gravitee plugin's IMAGE toolchain. A full apply/deploy needs
# a live Gravitee Management API, so this proves the parts that run WITHOUT a
# server and that the CVE bump (patched base + yq v4.54.2 on CPython 3.9) didn't
# break the toolchain:
#   - the graviteeio-cli fork (`gio`) still runs on the patched python:3.9 base,
#   - the bumped `yq` runs and is v4.54.2 (deep-merge tool the entrypoint needs),
#   - the bundled `gravitee-validate` security linter runs on the image's Python:
#     it PASSES a locked-down definition and BLOCKS an open one.
# (The wrapper's merge / env-substitution / token-off-argv behaviour is covered
# by entrypoint_test.sh against the real yq/jq.)
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-gravitee:smoke-$$"
W="$(mktemp -d)"
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; rm -rf "$W" 2>/dev/null || true; }
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> gio runs on the patched python:3.9 base"
OUT="$(docker run --rm --entrypoint gio "$IMG" --version 2>&1)" || die "gio --version failed" "$OUT"
grep -q "0.4.1" <<<"$OUT" || die "unexpected gio version" "$OUT"

echo "==> yq is the bumped v4.54.2 and functional"
OUT="$(docker run --rm --entrypoint yq "$IMG" --version 2>&1)" || die "yq --version failed" "$OUT"
grep -q "4.54.2" <<<"$OUT" || die "yq is not v4.54.2" "$OUT"
OUT="$(printf 'a:\n  b: 1\n' | docker run --rm -i --entrypoint yq "$IMG" -o=json '.a.b' 2>&1)" || die "yq run failed" "$OUT"
[ "$OUT" = "1" ] || die "yq did not evaluate the expression (got '$OUT')"

# gravitee-validate: a locked-down definition PASSES, an open one BLOCKS.
cat >"$W/clean.json" <<'JSON'
{ "paths": { "/orders": [ { "methods": ["GET","POST"], "jwt": {} } ] } }
JSON
cat >"$W/open.json" <<'JSON'
{ "paths": { "/orders": [ { "transform-headers": {} } ] } }
JSON

echo "==> gravitee-validate PASSES a locked-down definition"
set +e
OUT="$(docker run --rm -v "$W:/w" --entrypoint gravitee-validate "$IMG" \
  --methods block --auth block /w/clean.json 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "validator should pass a methods+auth-locked definition (rc=$RC)" "$OUT"

echo "==> gravitee-validate BLOCKS an open (no methods, no auth) definition"
set +e
OUT="$(docker run --rm -v "$W:/w" --entrypoint gravitee-validate "$IMG" \
  --methods block --auth block /w/open.json 2>&1)"; RC=$?
set -e
[ "$RC" -eq 2 ] || die "validator should BLOCK (exit 2) an open definition (rc=$RC)" "$OUT"
grep -q "no explicit methods" <<<"$OUT" || die "expected the open-methods finding" "$OUT"
grep -q "no auth policy" <<<"$OUT" || die "expected the missing-auth finding" "$OUT"

echo "PASS: gravitee smoke"
