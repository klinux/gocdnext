#!/usr/bin/env bash
# Hermetic unit tests for the google-play entrypoint. python3 is stubbed on PATH
# (no real Play API). Proves input validation and — the security-critical part —
# that the service-account JSON reaches publish.py via a 0600 file pointed at by
# GOOGLE_APPLICATION_CREDENTIALS and NEVER via argv. The Play edit transaction
# itself is covered by publish_test.py (run in-image by smoke_test.sh).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
ok()  { echo "ok: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- out ---"; printf '%s\n' "$2" | tail -20; }; fails=$((fails+1)); }

BIN="$TMP/bin"; mkdir -p "$BIN"
LOG="$TMP/py.log"
cat >"$BIN/python3" <<EOF
#!/usr/bin/env bash
: > "$LOG"
echo "PYARGV: \$*" >> "$LOG"
echo "CREDPATH: \${GOOGLE_APPLICATION_CREDENTIALS:-}" >> "$LOG"
if [ -f "\${GOOGLE_APPLICATION_CREDENTIALS:-/nonexistent}" ]; then
  echo "CREDMODE: \$(stat -c '%a' "\$GOOGLE_APPLICATION_CREDENTIALS")" >> "$LOG"
  echo "CREDCONTENT: \$(cat "\$GOOGLE_APPLICATION_CREDENTIALS")" >> "$LOG"
fi
exit 0
EOF
chmod +x "$BIN/python3"

SA_JSON='{"type":"service_account","private_key":"SUPER_SECRET_KEY_8821"}'

# run <workdir> KEY=VAL ...
run() {
    local wd="$1"; shift
    OUT="$( cd "$wd" && env PATH="$BIN:$PATH" GPLAY_PUBLISH="$TMP/publish.py" "$@" bash "$HERE/entrypoint.sh" 2>&1 )"
    RC=$?
}
newwd() { mktemp -d "$TMP/wd.XXXXXX"; }
mkapk() { local d; d="$(newwd)"; : > "$d/app.aab"; printf '%s' "$d"; }

# --- required-field validation -------------------------------------------
run "$(newwd)" PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=internal PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON"
{ [ "$RC" -eq 2 ] && grep -q 'package-name is required' <<<"$OUT"; } && ok "missing package-name → 2" || bad "missing package-name" "$OUT"

run "$(newwd)" PLUGIN_PACKAGE_NAME=com.x PLUGIN_TRACK=internal PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON"
{ [ "$RC" -eq 2 ] && grep -q 'artifact is required' <<<"$OUT"; } && ok "missing artifact → 2" || bad "missing artifact" "$OUT"

d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON"
{ [ "$RC" -eq 2 ] && grep -q 'track is required' <<<"$OUT"; } && ok "missing track → 2" || bad "missing track" "$OUT"

d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=internal
{ [ "$RC" -eq 2 ] && grep -q 'service-account-json is required' <<<"$OUT"; } && ok "missing SA json → 2" || bad "missing SA json" "$OUT"

# --- artifact checks ------------------------------------------------------
run "$(newwd)" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=missing.aab PLUGIN_TRACK=internal PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON"
{ [ "$RC" -eq 2 ] && grep -q 'artifact not found' <<<"$OUT"; } && ok "artifact not found → 2" || bad "artifact not found" "$OUT"

d="$(newwd)"; : > "$d/app.txt"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.txt PLUGIN_TRACK=internal PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON"
{ [ "$RC" -eq 2 ] && grep -q 'must be a .aab or .apk' <<<"$OUT"; } && ok "wrong extension → 2" || bad "wrong extension" "$OUT"

# --- status + rollout -----------------------------------------------------
d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=internal PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_STATUS=bogus
{ [ "$RC" -eq 2 ] && grep -q 'status must be' <<<"$OUT"; } && ok "invalid status → 2" || bad "invalid status" "$OUT"

d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=production PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_STATUS=inProgress
{ [ "$RC" -eq 2 ] && grep -q 'rollout is required' <<<"$OUT"; } && ok "inProgress without rollout → 2" || bad "inProgress without rollout" "$OUT"

for badr in 0 1 1.5 -0.2 abc; do
  d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=production PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_STATUS=inProgress PLUGIN_ROLLOUT="$badr"
  { [ "$RC" -eq 2 ] && grep -q 'rollout must be a number' <<<"$OUT"; } || { bad "rollout '$badr' should be rejected" "$OUT"; break; }
done
[ "$RC" -eq 2 ] && ok "invalid rollout values rejected"

d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=internal PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_MAPPING=nope.txt
{ [ "$RC" -eq 2 ] && grep -q 'mapping not found' <<<"$OUT"; } && ok "mapping not found → 2" || bad "mapping not found" "$OUT"

# --- happy path: SA json via 0600 file, NEVER on argv ---------------------
d="$(mkapk)"; run "$d" PLUGIN_PACKAGE_NAME=com.x PLUGIN_ARTIFACT=app.aab PLUGIN_TRACK=beta PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_STATUS=inProgress PLUGIN_ROLLOUT=0.25
calls="$(cat "$LOG" 2>/dev/null)"
if [ "$RC" -ne 0 ]; then bad "happy path should exit 0" "$OUT"
elif grep -qF 'SUPER_SECRET_KEY_8821' <<<"$(grep '^PYARGV:' <<<"$calls")"; then bad "SECRET LEAK: SA json on python argv" "$calls"
elif ! grep -q '^CREDPATH: /' <<<"$calls"; then bad "GOOGLE_APPLICATION_CREDENTIALS not set to a path" "$calls"
elif ! grep -q '^CREDMODE: 600' <<<"$calls"; then bad "credential file is not mode 600" "$calls"
elif ! grep -qF 'CREDCONTENT: '"$SA_JSON" <<<"$calls"; then bad "credential file content mismatch" "$calls"
else ok "happy path: SA json via 0600 file, off-argv"; fi

# credential tempfile is removed after the run (trap fires; no exec)
credpath="$(grep '^CREDPATH:' "$LOG" | awk '{print $2}')"
{ [ -n "$credpath" ] && [ ! -e "$credpath" ]; } && ok "credential tempfile removed on exit" || bad "credential tempfile leaked: $credpath"

echo "----"
if [ "$fails" -ne 0 ]; then echo "FAILED: $fails test(s)"; exit 1; fi
echo "PASS: google-play entrypoint"
