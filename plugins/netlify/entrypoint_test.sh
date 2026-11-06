#!/usr/bin/env bash
# Mock-PATH test for the netlify entrypoint — the required-input guards and the
# argv/env wiring without a real deploy (the real CLI + binary-removal contract
# is in smoke_test.sh). A mock `netlify` records its argv + the env it was given.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/netlify" <<'EOF'
#!/usr/bin/env bash
{ echo "ARGS=$*"; echo "TOKEN_ENV=${NETLIFY_AUTH_TOKEN:-<unset>}"; echo "SITE_ENV=${NETLIFY_SITE_ID:-<unset>}"; echo "PWD=$(pwd)"; } >> "$LOG"
case "$*" in *tok-sekret*|*site-sekret*) echo "ARGV_LEAK=yes" >> "$LOG";; esac
exit 0
EOF
chmod +x "$TMP/netlify"

fails=0
mkdir -p "$TMP/ws/site"
run() { local log="$1"; shift; : >"$log"; ( cd "$TMP/ws" && env "$@" LOG="$log" PATH="$TMP:$PATH" sh "$ENTRY" >"$log.out" 2>&1 ); echo $?; }
called() { [ -s "$1" ]; }

# 1. dir required → exit 2, netlify not called.
rc=$(run "$TMP/1.log" NETLIFY_AUTH_TOKEN=t PLUGIN_SITE_ID=s)
{ [ "$rc" = 2 ] && ! called "$TMP/1.log"; } || { echo "FAIL: missing dir should exit 2 without calling netlify (rc=$rc)"; fails=$((fails+1)); }

# 2. dir not found → exit 2.
rc=$(run "$TMP/2.log" PLUGIN_DIR=nope NETLIFY_AUTH_TOKEN=t PLUGIN_SITE_ID=s)
{ [ "$rc" = 2 ] && ! called "$TMP/2.log"; } || { echo "FAIL: missing dir path should exit 2 (rc=$rc)"; fails=$((fails+1)); }

# 3. token required → exit 2.
rc=$(run "$TMP/3.log" PLUGIN_DIR=site PLUGIN_SITE_ID=s)
{ [ "$rc" = 2 ] && ! called "$TMP/3.log"; } || { echo "FAIL: missing NETLIFY_AUTH_TOKEN should exit 2 (rc=$rc)"; fails=$((fails+1)); }

# 4. site-id required → exit 2.
rc=$(run "$TMP/4.log" PLUGIN_DIR=site NETLIFY_AUTH_TOKEN=t)
{ [ "$rc" = 2 ] && ! called "$TMP/4.log"; } || { echo "FAIL: missing site-id should exit 2 (rc=$rc)"; fails=$((fails+1)); }

# 5. happy path: argv is only paths/flags; token + site ride the env, not argv.
rc=$(run "$TMP/5.log" PLUGIN_DIR=site NETLIFY_AUTH_TOKEN=tok-sekret PLUGIN_SITE_ID=site-sekret)
[ "$rc" = 0 ] || { echo "FAIL: happy path should exit 0 (rc=$rc)"; cat "$TMP/5.log.out"; fails=$((fails+1)); }
grep -qx "ARGS=deploy --dir site --no-build" "$TMP/5.log" || { echo "FAIL: argv shape"; cat "$TMP/5.log"; fails=$((fails+1)); }
grep -qx "TOKEN_ENV=tok-sekret" "$TMP/5.log" || { echo "FAIL: token should reach the env"; cat "$TMP/5.log"; fails=$((fails+1)); }
grep -qx "SITE_ENV=site-sekret" "$TMP/5.log" || { echo "FAIL: site-id should reach the env (NETLIFY_SITE_ID)"; cat "$TMP/5.log"; fails=$((fails+1)); }
grep -q "ARGV_LEAK=yes" "$TMP/5.log" && { echo "FAIL: token/site leaked onto argv"; cat "$TMP/5.log"; fails=$((fails+1)); }

# 6. prod + alias flags.
rc=$(run "$TMP/6.log" PLUGIN_DIR=site NETLIFY_AUTH_TOKEN=t PLUGIN_SITE_ID=s PLUGIN_PROD=true PLUGIN_ALIAS=pr-7)
grep -qx "ARGS=deploy --dir site --no-build --prod --alias pr-7" "$TMP/6.log" || { echo "FAIL: prod/alias argv"; cat "$TMP/6.log"; fails=$((fails+1)); }

# 7. PLUGIN_WORKING_DIR honored.
mkdir -p "$TMP/ws/sub/site"
rc=$(run "$TMP/7.log" PLUGIN_WORKING_DIR="$TMP/ws/sub" PLUGIN_DIR=site NETLIFY_AUTH_TOKEN=t PLUGIN_SITE_ID=s)
grep -qx "PWD=$TMP/ws/sub" "$TMP/7.log" || { echo "FAIL: working-dir not honored"; cat "$TMP/7.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: netlify entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
