#!/usr/bin/env bash
# Mock-PATH test for the gcloud entrypoint — the argv/auth logic that needs no
# real GCP (the binary + offline commands are in smoke_test.sh). A mock `gcloud`
# records each call's argv and flags a raw-key leak; the entrypoint must require
# PLUGIN_COMMAND, word-split it correctly, append --project, and hand the SA key
# to `activate-service-account --key-file=` (never the JSON body on argv).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"; mkdir -p "$TMP/home"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/gcloud" <<'EOF'
#!/usr/bin/env bash
echo "ARGS=$*" >> "$LOG"
case "$*" in *"private_key"*|*"BEGIN "*) echo "LEAK=yes" >> "$LOG";; esac
exit 0
EOF
chmod +x "$TMP/gcloud"

fails=0
run() { local log="$1"; shift; : >"$log"; ( env "$@" HOME="$TMP/home" LOG="$log" PATH="$TMP:$PATH" bash "$ENTRY" >"$log.out" 2>&1 ); echo $?; }
called() { [ -s "$1" ]; }

# 1. PLUGIN_COMMAND required → exit 2, gcloud never called.
rc=$(run "$TMP/1.log")
[ "$rc" = 2 ] || { echo "FAIL: missing PLUGIN_COMMAND should exit 2 (got $rc)"; fails=$((fails+1)); }
called "$TMP/1.log" && { echo "FAIL: missing PLUGIN_COMMAND must not call gcloud"; fails=$((fails+1)); }

# 2. command is word-split and --project appended after it.
rc=$(run "$TMP/2.log" PLUGIN_COMMAND="run deploy svc --image gcr.io/p/app:1" PLUGIN_PROJECT=proj-x)
[ "$rc" = 0 ] || { echo "FAIL: valid command should exit 0 (got $rc)"; cat "$TMP/2.log.out"; fails=$((fails+1)); }
grep -qx "ARGS=run deploy svc --image gcr.io/p/app:1 --project proj-x" "$TMP/2.log" \
  || { echo "FAIL: argv word-split / --project passthrough"; cat "$TMP/2.log"; fails=$((fails+1)); }

# 3. no --project when PLUGIN_PROJECT unset.
rc=$(run "$TMP/3.log" PLUGIN_COMMAND="version")
grep -qx "ARGS=version" "$TMP/3.log" || { echo "FAIL: bare command argv"; cat "$TMP/3.log"; fails=$((fails+1)); }

# 4. SA key goes to activate-service-account --key-file= (a path), never the JSON
#    body on argv; the key-file lives under a tempdir.
rc=$(run "$TMP/4.log" PLUGIN_COMMAND="config list" \
      PLUGIN_CREDENTIALS_JSON='{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n","client_email":"a@b.iam.gserviceaccount.com"}')
[ "$rc" = 0 ] || { echo "FAIL: credentials path should exit 0 (got $rc)"; cat "$TMP/4.log.out"; fails=$((fails+1)); }
grep -qE "^ARGS=auth activate-service-account --key-file=.+/gcp-sa\.json$" "$TMP/4.log" \
  || { echo "FAIL: activate-service-account should use --key-file=<tempfile>"; cat "$TMP/4.log"; fails=$((fails+1)); }
grep -q "LEAK=yes" "$TMP/4.log" && { echo "FAIL: raw SA key material reached gcloud argv"; cat "$TMP/4.log"; fails=$((fails+1)); }
grep -qx "ARGS=config list" "$TMP/4.log" || { echo "FAIL: command should still run after activation"; cat "$TMP/4.log"; fails=$((fails+1)); }

# 5. PLUGIN_WORKING_DIR is honored (entrypoint cds there before running).
mkdir -p "$TMP/wd"
rc=$(run "$TMP/5.log" PLUGIN_COMMAND="version" PLUGIN_WORKING_DIR="$TMP/wd")
[ "$rc" = 0 ] || { echo "FAIL: working-dir should exit 0 (got $rc)"; cat "$TMP/5.log.out"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: gcloud entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
