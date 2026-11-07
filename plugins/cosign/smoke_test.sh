#!/usr/bin/env bash
# Real smoke for the cosign plugin's IMAGE. Real signing needs a registry + an
# OIDC issuer (keyless) or key material, so this proves what runs without them and
# that the major bump (cosign v2.4.1 -> v3.1.4) didn't drop the plugin's surface:
#   - the binary is v3.1.4,
#   - each action the plugin drives (verify / sign / attest, all keyless) runs the
#     REAL entrypoint and reaches cosign — failing on the network/registry/OIDC
#     boundary, NOT on a parse/unknown-flag error — which means the v3 CLI still
#     accepts the commands + flags the wrapper builds.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-cosign:smoke-$$"
W="$(mktemp -d)"
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; rm -rf "$W" 2>/dev/null || true; }
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> cosign is v3.1.4"
OUT="$(docker run --rm --entrypoint cosign "$IMG" version 2>&1)" || die "cosign version failed" "$OUT"
grep -q "v3.1.4" <<<"$OUT" || die "cosign is not v3.1.4" "$OUT"

# A bogus but well-formed digest ref: the action must reach cosign and fail on the
# network/registry/OIDC boundary, never on argument parsing.
ZERO="$(printf '0%.0s' $(seq 1 64))"
REF="example.invalid/nope@sha256:${ZERO}"
printf '{"predicate":"smoke"}\n' >"$W/pred.json"

run() { # run <extra -e ...> : sets RC + OUT
  set +e
  OUT="$(docker run --rm -v "$W:/workspace" -w /workspace -e PLUGIN_IMAGE="$REF" "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}
assert_reached_cosign() { # assert_reached_cosign <label>
  [ "$RC" -ne 0 ] || die "$1: expected a non-zero failure (no real registry/OIDC)" "$OUT"
  grep -q "==> cosign " <<<"$OUT" || die "$1: entrypoint did not invoke cosign" "$OUT"
  if grep -qiE 'unknown (flag|command|shorthand)|Error: accepts|requires at least|unknown sub' <<<"$OUT"; then
    die "$1: PARSE error — the cosign v3 CLI surface changed" "$OUT"
  fi
}

echo "==> verify (keyless) reaches cosign"
run -e PLUGIN_ACTION=verify -e PLUGIN_CERT_IDENTITY=https://x -e PLUGIN_CERT_OIDC_ISSUER=https://x
assert_reached_cosign "verify"

echo "==> sign (keyless) reaches cosign"
run -e PLUGIN_ACTION=sign
assert_reached_cosign "sign"

echo "==> attest (keyless) reaches cosign"
run -e PLUGIN_ACTION=attest -e PLUGIN_PREDICATE=pred.json -e PLUGIN_PREDICATE_TYPE=spdxjson
assert_reached_cosign "attest"

echo "PASS: cosign smoke"
