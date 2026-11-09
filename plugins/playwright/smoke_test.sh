#!/usr/bin/env bash
# Real smoke for the playwright plugin's IMAGE. Runs the PINNED image end to end
# so the base pin + npm bump that could move the CLI surface or regress the
# CVE-2026-59873 fix fails HERE, before publish. Proves:
#   - npm is 11.19.1 and EVERY node-tar bundled in it is >= 7.5.19 (the removed
#     CVE): the base shipped npm 10.9.2 with tar 6.2.1 + 7.4.3 (cacache/node-gyp);
#   - the plugin actually RUNS `playwright test` with the baked chromium: a real
#     minimal project is installed by the entrypoint and a test passes, writing
#     the JUnit report at the pinned path (test-results/junit.xml);
#   - a FAILING test makes the job exit non-zero (no false green — proves the
#     runner really executed, not that the wrapper swallowed the result).
#
# The browser half is offline (page.setContent, baked chromium-1155 matching
# playwright 1.50); only the dependency install needs network, exactly like a
# real job's install step. Convention-named so plugins.yml runs it as a
# PRE-PUBLISH gate. Built --no-cache to match the no_cache publish build. Trap
# cleanup (root-owned node_modules/test-results removed in-image), fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-playwright:smoke-$$"
# @playwright/test MUST match the browsers baked in the image (chromium-1155 =
# playwright 1.50) or the runner would try to download a browser (offline fail).
PW_VERSION="1.50.0"
WOK="$(mktemp -d)"   # passing project
WBAD="$(mktemp -d)"  # failing project
SMOKE_OK=0
cleanup() {
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint bash -v "$WOK:/a" -v "$WBAD:/b" "$IMG" \
      -c 'for d in /a /b; do rm -rf "$d"/* "$d"/.[!.]* "$d"/..?* 2>/dev/null || true; done' \
      >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WOK" "$WBAD" 2>/dev/null || true
  leftover=0
  for d in "$WOK" "$WBAD"; do
    [ -d "$d" ] && { echo "WARN: cleanup could not fully remove ${d}" >&2; leftover=1; }
  done
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Write a minimal playwright project into $1; $2 = expected <h1> text the test
# asserts (a mismatch makes the test fail — used for the negative case).
project() {
  mkdir -p "$1/tests"
  cat >"$1/package.json" <<JSON
{ "name": "pw-smoke", "private": true, "devDependencies": { "@playwright/test": "${PW_VERSION}" } }
JSON
  cat >"$1/playwright.config.ts" <<'TS'
import { defineConfig, devices } from '@playwright/test';
export default defineConfig({
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
TS
  cat >"$1/tests/smoke.spec.ts" <<TS
import { test, expect } from '@playwright/test';
test('renders with baked chromium', async ({ page }) => {
  await page.setContent('<h1 id="t">gocdnext</h1>');
  await expect(page.locator('#t')).toHaveText('$2');
});
TS
}
run() { d="$1"; shift; set +e; OUT="$(docker run --rm --network host -v "$d:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?; set -e; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> npm is 11.19.1 and every bundled node-tar is >= 7.5.19 (CVE-2026-59873 removed)"
NPMV="$(docker run --rm --network none --entrypoint npm "$IMG" --version 2>&1)" || die "npm --version failed" "$NPMV"
[ "$NPMV" = "11.19.1" ] || die "npm is not 11.19.1 (got: ${NPMV})"
# enumerate EVERY tar bundled under npm and assert each >= 7.5.19 (version sort:
# semver is not zero-padded, so the floor must sort first).
TARS="$(docker run --rm --network none --entrypoint bash "$IMG" -lc \
  'find /usr/lib/node_modules/npm -path "*/tar/package.json" -exec sh -c "grep -m1 \"\\\"version\\\"\" \"\$1\" | tr -d \" ,\\\"\" | cut -d: -f2" _ {} \;' 2>&1)"
[ -n "$TARS" ] || die "could not enumerate npm-bundled tar versions" "$TARS"
while IFS= read -r tv; do
  [ -z "$tv" ] && continue
  [ "$(printf '%s\n7.5.19\n' "$tv" | sort -V | head -1)" = "7.5.19" ] \
    || die "an npm-bundled tar (${tv}) is < 7.5.19 (CVE-2026-59873)" "$TARS"
done <<<"$TARS"

echo "==> plugin runs 'playwright test' with the baked chromium; JUnit lands at the pinned path"
project "$WOK" "gocdnext"           # test asserts the text it sets -> passes
run "$WOK"
[ "$RC" -eq 0 ] || die "passing project: expected exit 0, got $RC" "$OUT"
grep -q '1 passed' <<<"$OUT" || die "passing project: playwright did not report a pass" "$OUT"
[ -f "$WOK/test-results/junit.xml" ] || die "JUnit report not written at test-results/junit.xml" "$OUT"
grep -q '<testsuites' "$WOK/test-results/junit.xml" || die "JUnit report is not valid JUnit XML" "$OUT"

echo "==> a FAILING test makes the job exit non-zero (no false green)"
project "$WBAD" "WRONG"             # test expects 'WRONG' but the page says 'gocdnext' -> fails
run "$WBAD"
[ "$RC" -ne 0 ] || die "failing project: expected a non-zero exit, got 0 (false green)" "$OUT"
grep -q '1 failed' <<<"$OUT" || die "failing project: playwright did not report a failure" "$OUT"

SMOKE_OK=1
echo "PASS: playwright smoke"
