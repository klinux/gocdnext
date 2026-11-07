#!/usr/bin/env bash
# Real smoke for the cosign plugin's IMAGE. Signing an image needs a registry, and
# KEYLESS needs an interactive OIDC flow — so the smoke drives the KEY-BASED path
# with an EPHEMERAL local keypair and anchors every case on the deterministic
# NXDOMAIN of `example.invalid` (RFC2606). No OAuth, no network service, ~1s each.
# Proves the major bump (cosign v2.4.1 -> v3.1.4) didn't drop the plugin's surface:
#   - the binary is v3.1.4;
#   - verify / sign / attest with `--key` (+ COSIGN_PASSWORD) and `login` each run
#     the REAL entrypoint and reach cosign, failing on the registry DNS lookup —
#     NOT a parse/unknown-flag error;
#   - the key passphrase and the login password never appear in the log/argv.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-cosign:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  # cosign generate-key-pair + the runs write root-owned files into $W; remove
  # them from INSIDE the image (matching uid) before deleting it, then verify.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$W:/w" "$IMG" \
      -c 'rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  if [ -d "$W" ]; then
    echo "WARN: cleanup could not fully remove ${W}" >&2
    [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left files behind" >&2; exit 1; }
  fi
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

KEYPW="smoke-key-pw-$$"
LOGINPW="smoke-login-pw-$$"
ZERO="$(printf '0%.0s' $(seq 1 64))"
REF="example.invalid/nope@sha256:${ZERO}"

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> cosign is v3.1.4"
OUT="$(docker run --rm --entrypoint cosign "$IMG" version 2>&1)" || die "cosign version failed" "$OUT"
grep -q "v3.1.4" <<<"$OUT" || die "cosign is not v3.1.4" "$OUT"

echo "==> generate an ephemeral keypair (in-image)"
docker run --rm -v "$W:/workspace" -w /workspace -e COSIGN_PASSWORD="$KEYPW" \
  --entrypoint cosign "$IMG" generate-key-pair >/dev/null 2>&1 || die "generate-key-pair failed"
[ -f "$W/cosign.key" ] && [ -f "$W/cosign.pub" ] || die "keypair not written"
printf '{"predicate":"smoke"}\n' >"$W/pred.json"

run() { # run <extra -e ...> : sets RC + OUT
  set +e
  OUT="$(docker run --rm -v "$W:/workspace" -w /workspace -e PLUGIN_IMAGE="$REF" "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}
assert_dns() { # assert_dns <label> <secret-that-must-not-leak>
  [ "$RC" -ne 0 ] || die "$1: expected a non-zero failure" "$OUT"
  grep -q "==> cosign " <<<"$OUT" || die "$1: entrypoint did not invoke cosign" "$OUT"
  grep -q "lookup example.invalid" <<<"$OUT" \
    || die "$1: expected the deterministic example.invalid DNS failure" "$OUT"
  if grep -qiE 'unknown (flag|command|shorthand)|Error: accepts|requires at least|unknown sub' <<<"$OUT"; then
    die "$1: PARSE error — the cosign v3 CLI surface changed" "$OUT"
  fi
  grep -q "$2" <<<"$OUT" && die "$1: secret leaked into the log/argv" "$OUT" || true
}

echo "==> verify (key-based) reaches cosign"
run -e PLUGIN_ACTION=verify -e PLUGIN_KEY=cosign.pub
assert_dns "verify" "$KEYPW"

echo "==> sign (key-based, passphrase) reaches cosign; key pw not leaked"
run -e PLUGIN_ACTION=sign -e PLUGIN_KEY=cosign.key -e PLUGIN_KEY_PASSWORD="$KEYPW"
assert_dns "sign" "$KEYPW"

echo "==> attest (key-based) reaches cosign"
run -e PLUGIN_ACTION=attest -e PLUGIN_KEY=cosign.key -e PLUGIN_KEY_PASSWORD="$KEYPW" \
    -e PLUGIN_PREDICATE=pred.json -e PLUGIN_PREDICATE_TYPE=spdxjson
assert_dns "attest" "$KEYPW"

echo "==> login (registry creds) reaches cosign; login password not leaked"
run -e PLUGIN_ACTION=sign -e PLUGIN_KEY=cosign.key -e PLUGIN_KEY_PASSWORD="$KEYPW" \
    -e PLUGIN_REGISTRY=example.invalid -e PLUGIN_USERNAME=robot -e PLUGIN_PASSWORD="$LOGINPW"
assert_dns "login" "$LOGINPW"

SMOKE_OK=1
echo "PASS: cosign smoke"
