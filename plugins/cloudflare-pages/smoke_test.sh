#!/usr/bin/env bash
# Real smoke for the cloudflare-pages plugin's IMAGE. A real deploy needs a live
# Cloudflare account, so the smoke points wrangler's API base at the
# deterministic NXDOMAIN of `example.invalid` (RFC2606) via CLOUDFLARE_API_BASE_URL
# and anchors on the network failure. Proves the base/wrangler bump
# (node:22-alpine pinned + wrangler 3 -> 4.158.0) kept the plugin's surface:
#   - wrangler is 4.158.0 and on PATH (installed via `npm ci`);
#   - `wrangler pages deploy <dir> --project-name <p>` runs the REAL entrypoint
#     and reaches the Cloudflare API, failing on the network lookup — NOT a
#     parse/unknown-argument error;
#   - the API token never appears in the log.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-cloudflare-pages:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  if [ -d "$W" ]; then
    echo "WARN: cleanup could not fully remove ${W}" >&2
    [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left files behind" >&2; exit 1; }
  fi
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

TOKEN="tok-smoke-$$-SECRETMARKER"

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> wrangler is 4.158.0"
OUT="$(docker run --rm --entrypoint wrangler "$IMG" --version 2>&1)" || die "wrangler --version failed" "$OUT"
grep -q '4.158.0' <<<"$OUT" || die "wrangler is not 4.158.0" "$OUT"

echo "==> stage a built site dir"
mkdir -p "$W/site"; echo '<h1>smoke</h1>' >"$W/site/index.html"

# WRANGLER_LOG=debug surfaces the exact CF API request wrangler builds, so we can
# assert it reached the RIGHT Pages endpoint with the --branch flag honoured —
# not just a generic "fetch failed" that some other init could also produce.
echo "==> pages deploy (with --branch) reaches the exact Pages API; token not leaked; telemetry off"
set +e
OUT="$(docker run --rm -v "$W:/workspace" -w /workspace \
  -e PLUGIN_DIR=site -e PLUGIN_PROJECT_NAME=smoke -e PLUGIN_BRANCH=preview \
  -e CLOUDFLARE_API_TOKEN="$TOKEN" -e CLOUDFLARE_ACCOUNT_ID=acct-smoke \
  -e CLOUDFLARE_API_BASE_URL=https://example.invalid -e WRANGLER_LOG=debug "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || die "expected a non-zero failure" "$OUT"
grep -q '==> wrangler pages deploy' <<<"$OUT" || die "entrypoint did not invoke wrangler" "$OUT"
# Exact API call: proves wrangler 4 parsed `pages deploy --project-name --branch`,
# resolved the account, and hit the project endpoint at our stub host.
grep -qF 'CF API REQUEST: GET https://example.invalid/accounts/acct-smoke/pages/projects/smoke' <<<"$OUT" \
  || die "did not reach the exact Pages projects API endpoint" "$OUT"
# --branch must have been recognised (v4 surface), not silently dropped.
grep -qE '"argsUsed":\[[^]]*"branch"' <<<"$OUT" || die "--branch was not recognised by wrangler 4" "$OUT"
if grep -qiE 'unknown argument|not enough non-option|Unknown command|Usage:' <<<"$OUT"; then
  die "PARSE error — the wrangler CLI surface changed" "$OUT"
fi
grep -q 'SECRETMARKER' <<<"$OUT" && die "API token leaked into the log" "$OUT" || true
# Telemetry must be off: the dispatcher reports "disabled", never "Posting data".
grep -q 'Metrics dispatcher: Dispatching disabled' <<<"$OUT" \
  || die "could not confirm wrangler telemetry is disabled (WRANGLER_SEND_METRICS)" "$OUT"
grep -q 'Metrics dispatcher: Posting data' <<<"$OUT" && die "wrangler telemetry was POSTED despite WRANGLER_SEND_METRICS=false" "$OUT" || true

SMOKE_OK=1
echo "PASS: cloudflare-pages smoke"
