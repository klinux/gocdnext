#!/usr/bin/env bash
# Mock-level test for the cloudflare-pages entrypoint. NO docker, NO network: a
# fake `wrangler` on PATH records its argv, so we can assert the plugin's
# contract and its SECURITY property:
#
#   - the deploy argv is `pages deploy <dir> --project-name <p> [--branch <b>]`;
#   - the API token and account id ride the ENVIRONMENT (wrangler's native
#     contract) and NEVER reach argv (visible via /proc/<pid>/cmdline);
#   - required inputs fail fast (exit 2) without invoking wrangler at all.
#
# Convention-named so plugins.yml runs it as a pre-publish gate (mock stage).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="${HERE}/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

BIN="${TMP}/bin"; mkdir -p "${BIN}"
export ARGV_LOG="${TMP}/argv.log"
cat >"${BIN}/wrangler" <<'EOF'
#!/usr/bin/env bash
printf 'wrangler %s\n' "$*" >>"${ARGV_LOG}"
exit 0
EOF
chmod +x "${BIN}/wrangler"
export PATH="${BIN}:${PATH}"

WS="${TMP}/ws"; mkdir -p "${WS}/site"; echo '<h1>hi</h1>' >"${WS}/site/index.html"
TOKEN='tok-SECRETMARKER'
ACCT='acct-ACCTMARKER'

fail() { echo "FAIL: $1"; echo "--- argv ---"; cat "${ARGV_LOG}" 2>/dev/null; exit 1; }

echo "==> happy path: deploy argv is well-formed; token/account stay in env"
: >"${ARGV_LOG}"
( cd "${WS}" && env PLUGIN_DIR=site PLUGIN_PROJECT_NAME=smoke PLUGIN_BRANCH=preview \
    CLOUDFLARE_API_TOKEN="${TOKEN}" CLOUDFLARE_ACCOUNT_ID="${ACCT}" \
    sh "${ENTRY}" >/dev/null 2>&1 )
grep -q 'pages deploy site --project-name smoke --branch preview' "${ARGV_LOG}" \
  || fail "deploy argv is not the expected shape"
grep -q 'SECRETMARKER' "${ARGV_LOG}" && fail "API token leaked into argv"
grep -q 'ACCTMARKER' "${ARGV_LOG}" && fail "account id leaked into argv"

echo "==> no --branch when PLUGIN_BRANCH is unset"
: >"${ARGV_LOG}"
( cd "${WS}" && env PLUGIN_DIR=site PLUGIN_PROJECT_NAME=smoke \
    CLOUDFLARE_API_TOKEN="${TOKEN}" CLOUDFLARE_ACCOUNT_ID="${ACCT}" \
    sh "${ENTRY}" >/dev/null 2>&1 )
grep -q 'pages deploy site --project-name smoke$' "${ARGV_LOG}" \
  || fail "deploy argv should omit --branch when unset"

expect_fail2() { # expect_fail2 <label> <grep-in-stderr> <env...>
  local label="$1" needle="$2"; shift 2
  : >"${ARGV_LOG}"
  local out rc
  set +e
  out="$( cd "${WS}" && env "$@" sh "${ENTRY}" 2>&1 )"; rc=$?
  set -e
  [ "$rc" -eq 2 ] || fail "${label}: expected exit 2, got ${rc}"
  grep -q "${needle}" <<<"${out}" || fail "${label}: missing message '${needle}' (got: ${out})"
  [ -s "${ARGV_LOG}" ] && fail "${label}: wrangler was invoked despite a bad input"
  return 0
}

echo "==> required-input guards fail fast, before wrangler"
expect_fail2 "no project" "project-name: is required" \
  PLUGIN_DIR=site CLOUDFLARE_API_TOKEN="${TOKEN}" CLOUDFLARE_ACCOUNT_ID="${ACCT}"
expect_fail2 "no token" "CLOUDFLARE_API_TOKEN" \
  PLUGIN_DIR=site PLUGIN_PROJECT_NAME=smoke CLOUDFLARE_ACCOUNT_ID="${ACCT}"
expect_fail2 "no account" "CLOUDFLARE_ACCOUNT_ID" \
  PLUGIN_DIR=site PLUGIN_PROJECT_NAME=smoke CLOUDFLARE_API_TOKEN="${TOKEN}"
expect_fail2 "missing dir" "not found" \
  PLUGIN_DIR=nope PLUGIN_PROJECT_NAME=smoke CLOUDFLARE_API_TOKEN="${TOKEN}" CLOUDFLARE_ACCOUNT_ID="${ACCT}"

echo "PASS: cloudflare-pages entrypoint mock test"
