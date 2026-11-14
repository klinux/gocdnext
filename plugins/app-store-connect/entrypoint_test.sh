#!/usr/bin/env bash
# Hermetic entrypoint contract tests. Python is stubbed: these prove validation,
# secret materialization, off-argv handling, failure propagation and cleanup.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="${TMP}/bin"
LOG="${TMP}/python.log"
mkdir -p "$BIN"

cat >"${BIN}/python3" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
: >"$MOCK_LOG"
printf 'PYARGV:' >>"$MOCK_LOG"
printf ' <%s>' "$@" >>"$MOCK_LOG"
printf '\n' >>"$MOCK_LOG"
printf 'KEYPATH: %s\n' "${ASC_KEY_PATH:-}" >>"$MOCK_LOG"
if [ -f "${ASC_KEY_PATH:-/nonexistent}" ]; then
  printf 'KEYMODE: %s\n' "$(stat -c '%a' "$ASC_KEY_PATH")" >>"$MOCK_LOG"
  printf 'KEYHASH: %s\n' "$(sha256sum "$ASC_KEY_PATH" | awk '{print $1}')" >>"$MOCK_LOG"
fi
printf 'NOTESHASH: %s\n' "$(printf '%s' "${PLUGIN_RELEASE_NOTES:-}" | sha256sum | awk '{print $1}')" >>"$MOCK_LOG"
exit "${MOCK_PYTHON_RC:-0}"
STUB
chmod +x "${BIN}/python3"

API_KEY='-----BEGIN PRIVATE KEY-----
entrypoint-secret-key
-----END PRIVATE KEY-----'
API_KEY_B64="$(printf '%s' "$API_KEY" | base64 -w 0)"
API_KEY_HASH="$(printf '%s' "$API_KEY" | sha256sum | awk '{print $1}')"

newwd() { mktemp -d "${TMP}/wd.XXXXXX"; }
mkipa() { local d; d="$(newwd)"; : >"${d}/App.ipa"; printf '%s' "$d"; }

run() {
  local wd="$1"; shift
  : >"$LOG"
  OUT="$(cd "$wd" && env \
    PATH="${BIN}:/usr/bin:/bin" \
    ASC_PUBLISH="${TMP}/publish.py" \
    MOCK_LOG="$LOG" \
    "$@" bash "${HERE}/entrypoint.sh" 2>&1)"
  RC=$?
}

base_env=(
  PLUGIN_APP_ID=123456789
  PLUGIN_IPA=App.ipa
  PLUGIN_API_KEY_ID=KEY123
  PLUGIN_API_ISSUER_ID=issuer-uuid
  PLUGIN_API_KEY="$API_KEY_B64"
)

fails=0
ok() { printf 'ok: %s\n' "$1"; }
bad() { printf 'FAIL: %s\n%s\n' "$1" "${2:-}"; fails=$((fails + 1)); }

run "$(newwd)" PLUGIN_IPA=App.ipa PLUGIN_API_KEY_ID=x PLUGIN_API_ISSUER_ID=y PLUGIN_API_KEY="$API_KEY_B64"
{ [ "$RC" -eq 2 ] && grep -q 'app-id is required' <<<"$OUT"; } && ok "missing app-id rejected" || bad "missing app-id" "$OUT"

d="$(mkipa)"; run "$d" PLUGIN_APP_ID=1 PLUGIN_IPA=App.ipa PLUGIN_API_ISSUER_ID=y PLUGIN_API_KEY="$API_KEY_B64"
{ [ "$RC" -eq 2 ] && grep -q 'api-key-id is required' <<<"$OUT"; } && ok "missing key id rejected" || bad "missing key id" "$OUT"

d="$(mkipa)"; run "$d" PLUGIN_APP_ID=1 PLUGIN_IPA=App.ipa PLUGIN_API_KEY_ID=x PLUGIN_API_KEY="$API_KEY_B64"
{ [ "$RC" -eq 2 ] && grep -q 'api-issuer-id is required' <<<"$OUT"; } && ok "missing issuer rejected" || bad "missing issuer" "$OUT"

d="$(mkipa)"; run "$d" PLUGIN_APP_ID=1 PLUGIN_IPA=App.ipa PLUGIN_API_KEY_ID=x PLUGIN_API_ISSUER_ID=y
{ [ "$RC" -eq 2 ] && grep -q 'api-key is required' <<<"$OUT"; } && ok "missing private key rejected" || bad "missing private key" "$OUT"

run "$(newwd)" "${base_env[@]}"
{ [ "$RC" -eq 2 ] && grep -q 'ipa not found' <<<"$OUT"; } && ok "missing IPA rejected" || bad "missing IPA" "$OUT"

d="$(newwd)"; : >"${d}/App.zip"; run "$d" "${base_env[@]/PLUGIN_IPA=App.ipa/PLUGIN_IPA=App.zip}"
{ [ "$RC" -eq 2 ] && grep -q 'must end in .ipa' <<<"$OUT"; } && ok "wrong artifact extension rejected" || bad "wrong extension" "$OUT"

d="$(mkipa)"; run "$d" "${base_env[@]}" PLUGIN_TRACK=store
{ [ "$RC" -eq 2 ] && grep -q 'track must be upload|testflight' <<<"$OUT"; } && ok "unknown track rejected" || bad "unknown track" "$OUT"

d="$(mkipa)"; run "$d" "${base_env[@]}" PLUGIN_RELEASE_NOTES=notes
{ [ "$RC" -eq 2 ] && grep -q 'require track: testflight' <<<"$OUT"; } && ok "TestFlight fields on upload track rejected" || bad "track field mismatch" "$OUT"

d="$(mkipa)"; run "$d" "${base_env[@]}" PLUGIN_PLATFORM=MAC_OS
{ [ "$RC" -eq 2 ] && grep -q 'platform must be' <<<"$OUT"; } && ok "non-IPA platform rejected" || bad "platform validation" "$OUT"

d="$(mkipa)"; run "$d" "${base_env[@]}" PLUGIN_TRACK=testflight PLUGIN_LANGUAGE='not_a_locale'
{ [ "$RC" -eq 2 ] && grep -q 'language must be' <<<"$OUT"; } && ok "bad locale rejected" || bad "locale validation" "$OUT"

d="$(mkipa)"; run "$d" "${base_env[@]}" PLUGIN_TRACK=testflight PLUGIN_WAIT_TIMEOUT=0
{ [ "$RC" -eq 2 ] && grep -q 'wait-timeout must be' <<<"$OUT"; } && ok "bad timeout rejected" || bad "timeout validation" "$OUT"

d="$(mkipa)"; run "$d" "${base_env[@]/PLUGIN_API_KEY=$API_KEY_B64/PLUGIN_API_KEY=not-base64!}"
{ [ "$RC" -eq 2 ] && grep -q 'not valid base64' <<<"$OUT"; } && ok "invalid private key base64 rejected" || bad "base64 validation" "$OUT"

notes='private release notes that must stay off argv and output'
notes_hash="$(printf '%s' "$notes" | sha256sum | awk '{print $1}')"
d="$(mkipa)"; run "$d" "${base_env[@]}" \
  PLUGIN_TRACK=testflight PLUGIN_BETA_GROUPS='Internal QA,Beta' \
  PLUGIN_RELEASE_NOTES="$notes" PLUGIN_LANGUAGE=pt-BR
calls="$(cat "$LOG")"
key_path="$(awk '/^KEYPATH:/ {print $2}' "$LOG")"
if [ "$RC" -ne 0 ]; then
  bad "happy path should exit zero" "$OUT"
elif ! grep -q '^PYARGV: <.*/publish.py>$' <<<"$calls"; then
  bad "python argv must contain only the publisher path" "$calls"
elif ! grep -q '^KEYMODE: 600$' <<<"$calls" || ! grep -q "^KEYHASH: ${API_KEY_HASH}$" <<<"$calls"; then
  bad "private key must be exact and mode 0600" "$calls"
elif ! grep -q "^NOTESHASH: ${notes_hash}$" <<<"$calls"; then
  bad "release notes must reach the publisher through the environment" "$calls"
elif [ -e "$key_path" ] || [ -d "$(dirname "$key_path")" ]; then
  bad "private key tempdir must be removed" "$key_path"
elif grep -Fq 'entrypoint-secret-key' <<<"${OUT}${calls%%KEYHASH:*}" || \
     grep -Fq "$API_KEY_B64" <<<"${OUT}${calls}" || grep -Fq "$notes" <<<"${OUT}${calls}"; then
  bad "credential or release notes leaked to output/argv" "${OUT}${calls}"
else
  ok "happy path keeps .p8 and notes off argv/log and removes tempfile"
fi

d="$(mkipa)"; run "$d" "${base_env[@]}" MOCK_PYTHON_RC=41
failed_key_path="$(awk '/^KEYPATH:/ {print $2}' "$LOG")"
if [ "$RC" -ne 41 ]; then
  bad "publisher failure must propagate" "$OUT"
elif [ -e "$failed_key_path" ] || [ -d "$(dirname "$failed_key_path")" ]; then
  bad "publisher failure must still remove private key" "$failed_key_path"
else
  ok "publisher failure propagates and cleans key"
fi

printf '%s\n' '----'
if [ "$fails" -ne 0 ]; then
  printf 'FAILED: %d test(s)\n' "$fails" >&2
  exit 1
fi
printf 'PASS: app-store-connect entrypoint\n'
