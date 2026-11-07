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
assert_reached_api() { # assert_reached_api <label>
  [ "$RC" -ne 0 ] || die "$1: expected a non-zero auth failure (rc=$RC)" "$OUT"
  grep -qiE 'token .*is not valid|not authorized|Retrieving project' <<<"$OUT" \
    || die "$1: expected the CLI to reach the Vercel API and reject the token" "$OUT"
  if grep -qiE 'Cannot find module|SyntaxError|is not a function|Unknown command|command not found' <<<"$OUT"; then
    die "$1: got a module/parse error — the CLI or deploy path failed to load" "$OUT"
  fi
}

echo "==> entrypoint 'deploy' (preview) loads + reaches the Vercel API"
runep
assert_reached_api "deploy (preview)"

echo "==> entrypoint 'deploy --prod' is accepted the same way"
runep -e PLUGIN_PROD=true
assert_reached_api "deploy --prod"
grep -q "prod=true" <<<"$OUT" || die "the --prod path was not taken" "$OUT"

echo "PASS: vercel smoke"
