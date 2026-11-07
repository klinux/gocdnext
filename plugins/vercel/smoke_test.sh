#!/usr/bin/env bash
# Real smoke for the vercel plugin's IMAGE. A real deploy needs valid Vercel
# credentials + network, so this proves what runs without them and that the major
# CLI bump (v37 -> v65) + the node-tar override didn't break the deploy path:
#   - the Vercel CLI is v65.12.1 and loads,
#   - the real entrypoint, given a format-valid but bogus token, reaches the
#     Vercel API and fails with an AUTH error — not a module/parse error — which
#     means the CLI + the deploy code path (which pulls in the overridden tar)
#     loaded and the entrypoint built the command correctly,
#   - the --prod path is accepted the same way.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker +
# outbound network (the CLI contacts api.vercel.com to reject the token). Built
# --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-vercel:smoke-$$"
W="$(mktemp -d)"
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; rm -rf "$W" 2>/dev/null || true; }
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> Vercel CLI is v65.12.1 and loads"
OUT="$(docker run --rm --entrypoint vercel "$IMG" --version 2>&1)" || die "vercel --version failed" "$OUT"
grep -q "65.12.1" <<<"$OUT" || die "vercel is not v65.12.1" "$OUT"

# A tiny project for the CLI to act on.
printf '<!doctype html><title>smoke</title><h1>hi</h1>\n' >"$W/index.html"

runep() { # runep <extra -e ...> : sets RC + OUT. Token is format-valid but bogus.
  set +e
  OUT="$(docker run --rm -v "$W:/workspace" -w /workspace \
    -e VERCEL_TOKEN=smokefaketoken0123456789 -e VERCEL_ORG_ID=team_smoke -e VERCEL_PROJECT_ID=prj_smoke \
    "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}
# The entrypoint unsets VERCEL_TOKEN and authenticates from auth.json, so the CLI
# returns its stable FILE-path rejection. Anchor on exactly that — NOT on
# "Retrieving project" (which also precedes a later TLS/network error, a false
# green) and NOT on the env-var message (which would mean auth.json was bypassed).
assert_file_auth() { # assert_file_auth <label>
  [ "$RC" -ne 0 ] || die "$1: expected a non-zero auth failure (rc=$RC)" "$OUT"
  grep -q "The specified token is not valid" <<<"$OUT" \
    || die "$1: expected the file-based 'The specified token is not valid' rejection" "$OUT"
  if grep -qiE 'VERCEL_TOKEN environment variable' <<<"$OUT"; then
    die "$1: CLI used the ENV token — auth.json was bypassed (VERCEL_TOKEN not unset)" "$OUT"
  fi
  if grep -qiE 'Cannot find module|SyntaxError|is not a function|Unknown command|command not found' <<<"$OUT"; then
    die "$1: got a module/parse error — the CLI or deploy path failed to load" "$OUT"
  fi
}

echo "==> entrypoint 'deploy' (preview) authenticates from auth.json and reaches the API"
runep
assert_file_auth "deploy (preview)"

echo "==> entrypoint 'deploy --prod' is accepted the same way"
runep -e PLUGIN_PROD=true
assert_file_auth "deploy --prod"
grep -q "prod=true" <<<"$OUT" || die "the --prod path was not taken" "$OUT"

echo "PASS: vercel smoke"
