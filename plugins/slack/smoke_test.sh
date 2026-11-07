#!/usr/bin/env bash
# Real smoke for the slack plugin's IMAGE. Posting to Slack needs a live webhook,
# so the smoke points PLUGIN_WEBHOOK at the deterministic NXDOMAIN of
# `example.invalid` (RFC2606) with a token in the URL path, and anchors on the
# network failure. Proves the toolchain/base bump (golang:1.23 -> 1.27,
# alpine:3.20 -> 3.24.4) kept the plugin working AND that the SECURITY contract
# holds:
#   - the binary runs, reads PLUGIN_*/CI_* env, builds the request and reaches
#     the network, failing on the DNS lookup — NOT a crash/panic;
#   - the webhook URL's secret token NEVER appears in the log, even on error;
#   - a missing webhook fails fast (exit 2).
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-slack:smoke-$$"
SMOKE_OK=0
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; }
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

TOKEN="SMOKETOKEN-$$"
WEBHOOK="https://example.invalid/services/T000/B000/${TOKEN}"

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> missing webhook fails fast (exit 2)"
set +e
OUT="$(docker run --rm "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 2 ] || die "missing-webhook: expected exit 2, got $RC" "$OUT"
grep -q 'PLUGIN_WEBHOOK is required' <<<"$OUT" || die "missing-webhook: wrong message" "$OUT"

echo "==> post reaches the network; token not leaked; no panic"
set +e
OUT="$(docker run --rm \
  -e PLUGIN_WEBHOOK="$WEBHOOK" \
  -e CI_PIPELINE=demo -e CI_RUN_COUNTER=7 -e CI_PIPELINE_STATUS=success -e CI_COMMIT_SHA=abc1234 \
  "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 1 ] || die "post: expected exit 1 on DNS failure, got $RC" "$OUT"
grep -q 'slack post failed' <<<"$OUT" || die "post: did not reach the post path" "$OUT"
grep -qiE 'no such host|lookup example.invalid|dial tcp' <<<"$OUT" \
  || die "post: expected the deterministic example.invalid DNS failure" "$OUT"
grep -qi 'panic' <<<"$OUT" && die "post: the binary panicked" "$OUT" || true
grep -q "$TOKEN" <<<"$OUT" && die "post: webhook token leaked into the log" "$OUT" || true

SMOKE_OK=1
echo "PASS: slack smoke"
