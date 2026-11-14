#!/usr/bin/env bash
# Hermetic unit tests for the firebase-app-distribution entrypoint. The firebase
# CLI is stubbed on PATH. Proves input validation and that the service-account
# JSON + release notes reach the CLI via files (0600 credential pointed at by
# GOOGLE_APPLICATION_CREDENTIALS; notes via --release-notes-file) and NEVER via
# argv. A real distribute needs a live Firebase project, so it isn't exercised
# here — the smoke only proves the CLI + subcommand exist in the image.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
ok()  { echo "ok: $1"; }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- out ---"; printf '%s\n' "$2" | tail -20; }; fails=$((fails+1)); }

BIN="$TMP/bin"; mkdir -p "$BIN"
LOG="$TMP/fb.log"
cat >"$BIN/firebase" <<EOF
#!/usr/bin/env bash
: > "$LOG"
echo "ARGV: \$*" >> "$LOG"
echo "CREDPATH: \${GOOGLE_APPLICATION_CREDENTIALS:-}" >> "$LOG"
if [ -f "\${GOOGLE_APPLICATION_CREDENTIALS:-/nonexistent}" ]; then
  echo "CREDMODE: \$(stat -c '%a' "\$GOOGLE_APPLICATION_CREDENTIALS")" >> "$LOG"
  echo "CREDCONTENT: \$(cat "\$GOOGLE_APPLICATION_CREDENTIALS")" >> "$LOG"
fi
# capture release-notes file content if the flag is present
prev=""
for a in "\$@"; do
  [ "\$prev" = "--release-notes-file" ] && echo "NOTES: \$(cat "\$a")" >> "$LOG"
  prev="\$a"
done
exit 0
EOF
chmod +x "$BIN/firebase"

SA_JSON='{"type":"service_account","private_key":"FB_SECRET_KEY_5567"}'

run() {
    local wd="$1"; shift
    OUT="$( cd "$wd" && env PATH="$BIN:$PATH" FIREBASE_BIN=firebase "$@" bash "$HERE/entrypoint.sh" 2>&1 )"
    RC=$?
}
newwd() { mktemp -d "$TMP/wd.XXXXXX"; }
mkapk() { local d; d="$(newwd)"; : > "$d/app.apk"; printf '%s' "$d"; }

APP=1:1234567890:android:abcdef

# --- required-field validation -------------------------------------------
run "$(newwd)" PLUGIN_ARTIFACT=app.apk PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_GROUPS=qa
{ [ "$RC" -eq 2 ] && grep -q 'app-id is required' <<<"$OUT"; } && ok "missing app-id → 2" || bad "missing app-id" "$OUT"

run "$(newwd)" PLUGIN_APP_ID="$APP" PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_GROUPS=qa
{ [ "$RC" -eq 2 ] && grep -q 'artifact is required' <<<"$OUT"; } && ok "missing artifact → 2" || bad "missing artifact" "$OUT"

d="$(mkapk)"; run "$d" PLUGIN_APP_ID="$APP" PLUGIN_ARTIFACT=app.apk PLUGIN_GROUPS=qa
{ [ "$RC" -eq 2 ] && grep -q 'service-account-json is required' <<<"$OUT"; } && ok "missing SA json → 2" || bad "missing SA json" "$OUT"

d="$(mkapk)"; run "$d" PLUGIN_APP_ID="$APP" PLUGIN_ARTIFACT=app.apk PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON"
{ [ "$RC" -eq 2 ] && grep -q 'at least one of groups or testers' <<<"$OUT"; } && ok "no groups/testers → 2" || bad "no groups/testers" "$OUT"

# --- artifact checks ------------------------------------------------------
run "$(newwd)" PLUGIN_APP_ID="$APP" PLUGIN_ARTIFACT=missing.apk PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_GROUPS=qa
{ [ "$RC" -eq 2 ] && grep -q 'artifact not found' <<<"$OUT"; } && ok "artifact not found → 2" || bad "artifact not found" "$OUT"

d="$(newwd)"; : > "$d/app.txt"; run "$d" PLUGIN_APP_ID="$APP" PLUGIN_ARTIFACT=app.txt PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" PLUGIN_GROUPS=qa
{ [ "$RC" -eq 2 ] && grep -q 'must be a .aab or .apk' <<<"$OUT"; } && ok "wrong extension → 2" || bad "wrong extension" "$OUT"

# --- happy path: creds via 0600 file + notes via file, NEVER on argv ------
d="$(mkapk)"; run "$d" PLUGIN_APP_ID="$APP" PLUGIN_ARTIFACT=app.apk PLUGIN_SERVICE_ACCOUNT_JSON="$SA_JSON" \
    PLUGIN_GROUPS="qa,beta" PLUGIN_RELEASE_NOTES=$'line one\nline two'
calls="$(cat "$LOG" 2>/dev/null)"
argv="$(grep '^ARGV:' <<<"$calls")"
if [ "$RC" -ne 0 ]; then bad "happy path should exit 0" "$OUT"
elif ! grep -q 'appdistribution:distribute app.apk --app '"$APP" <<<"$argv"; then bad "firebase not called with artifact + --app" "$calls"
elif ! grep -q -- '--groups qa,beta' <<<"$argv"; then bad "groups not passed" "$calls"
elif grep -qF 'FB_SECRET_KEY_5567' <<<"$argv"; then bad "SECRET LEAK: SA json on firebase argv" "$calls"
elif ! grep -q '^CREDMODE: 600' <<<"$calls"; then bad "credential file not 0600" "$calls"
elif ! grep -qF 'CREDCONTENT: '"$SA_JSON" <<<"$calls"; then bad "credential content mismatch" "$calls"
elif grep -qF 'line one' <<<"$argv"; then bad "SECRET/UX: release notes text leaked onto argv" "$calls"
elif ! grep -q -- '--release-notes-file ' <<<"$argv"; then bad "release notes not passed via file" "$calls"
elif ! grep -qzF $'NOTES: line one\nline two' <<<"$calls"; then bad "release notes file content mismatch" "$calls"
else ok "happy path: creds + notes via files, off-argv"; fi

credpath="$(grep '^CREDPATH:' "$LOG" | awk '{print $2}')"
{ [ -n "$credpath" ] && [ ! -e "$credpath" ]; } && ok "credential tempfile removed on exit" || bad "credential tempfile leaked: $credpath"

echo "----"
if [ "$fails" -ne 0 ]; then echo "FAILED: $fails test(s)"; exit 1; fi
echo "PASS: firebase-app-distribution entrypoint"
