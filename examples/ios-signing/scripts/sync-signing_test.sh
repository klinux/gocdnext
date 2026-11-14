#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="${HERE}/sync-signing.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="${TMP}/bin"
CALLS="${TMP}/calls.log"
KEY_PATH_CAPTURE="${TMP}/key-path"
KEY_MODE_CAPTURE="${TMP}/key-mode"
KEY_HASH_CAPTURE="${TMP}/key-hash"
mkdir -p "$BIN" "${TMP}/home/.config/gocdnext"
touch "$CALLS"

cat >"${TMP}/home/.config/gocdnext/ios-toolchain.env" <<EOF
export PATH=${BIN}:/usr/bin:/bin
EOF

cat >"${BIN}/bundle" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'bundle' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
printf '%s\n' "$ASC_KEY_PATH" >"$MOCK_KEY_PATH_CAPTURE"
stat -c '%a' "$ASC_KEY_PATH" >"$MOCK_KEY_MODE_CAPTURE"
sha256sum "$ASC_KEY_PATH" | awk '{ print $1 }' >"$MOCK_KEY_HASH_CAPTURE"
[ -n "$MATCH_PASSWORD" ]
[ -n "$GOCDNEXT_TEMP_KEYCHAIN_PASSWORD" ]
[ "${MOCK_BUNDLE_FAIL:-0}" != "1" ] || exit 42
STUB

cat >"${BIN}/openssl" <<'STUB'
#!/usr/bin/env bash
printf 'generated-keychain-password-that-is-never-logged\n'
STUB

cat >"${BIN}/security" <<'STUB'
#!/usr/bin/env bash
printf 'security' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
STUB
chmod +x "${BIN}"/*

secret_p8='-----BEGIN PRIVATE KEY-----
super-secret-test-key
-----END PRIVATE KEY-----'
secret_b64="$(printf '%s' "$secret_p8" | base64 -w 0)"
OUTPUT="$(env \
  PATH="${BIN}:/usr/bin:/bin" \
  HOME="${TMP}/home" \
  TMPDIR="$TMP" \
  MOCK_CALLS="$CALLS" \
  MOCK_KEY_PATH_CAPTURE="$KEY_PATH_CAPTURE" \
  MOCK_KEY_MODE_CAPTURE="$KEY_MODE_CAPTURE" \
  MOCK_KEY_HASH_CAPTURE="$KEY_HASH_CAPTURE" \
  ASC_KEY_ID=ABC123 \
  ASC_ISSUER_ID=issuer-123 \
  ASC_KEY_P8_BASE64="$secret_b64" \
  MATCH_GIT_URL=git@example.invalid:signing.git \
  MATCH_PASSWORD='match-password-secret' \
  bash "$SCRIPT" 2>&1)"
RC=$?

fails=0
bad() { printf 'FAIL: %s\n%s\n' "$1" "${2:-}"; fails=$((fails + 1)); }
calls="$(cat "$CALLS")"
key_path="$(cat "$KEY_PATH_CAPTURE" 2>/dev/null || true)"
want_hash="$(printf '%s' "$secret_p8" | sha256sum | awk '{ print $1 }')"

if [ "$RC" -ne 0 ]; then
  bad "signing wrapper should succeed" "$OUTPUT"
fi
if ! grep -q '^bundle <exec> <fastlane> <ios> <sync_signing>$' <<<"$calls"; then
  bad "fastlane argv must contain only the lane name" "$calls"
fi
if [ "$(cat "$KEY_MODE_CAPTURE" 2>/dev/null || true)" != "600" ]; then
  bad "materialized .p8 must be mode 0600"
fi
if [ "$(cat "$KEY_HASH_CAPTURE" 2>/dev/null || true)" != "$want_hash" ]; then
  bad "materialized .p8 content differs from the supplied secret"
fi
if [ -n "$key_path" ] && { [ -e "$key_path" ] || [ -d "$(dirname "$key_path")" ]; }; then
  bad "temporary key and directory must be removed on EXIT" "$key_path"
fi
if grep -Fq 'super-secret-test-key' <<<"${OUTPUT}${calls}" || \
   grep -Fq 'match-password-secret' <<<"${OUTPUT}${calls}" || \
   grep -Fq "$secret_b64" <<<"${OUTPUT}${calls}"; then
  bad "secret leaked to output or argv" "${OUTPUT}${calls}"
fi
if ! grep -q '^security <delete-keychain> <gocdnext-' <<<"$calls"; then
  bad "wrapper must attempt keychain cleanup" "$calls"
fi

# A fastlane/match failure must still execute the shell EXIT cleanup. Use new
# captures so this assertion cannot accidentally pass on the successful run.
: >"$CALLS"
rm -f "$KEY_PATH_CAPTURE" "$KEY_MODE_CAPTURE" "$KEY_HASH_CAPTURE"
FAIL_OUTPUT="$(env \
  PATH="${BIN}:/usr/bin:/bin" \
  HOME="${TMP}/home" \
  TMPDIR="$TMP" \
  MOCK_CALLS="$CALLS" \
  MOCK_KEY_PATH_CAPTURE="$KEY_PATH_CAPTURE" \
  MOCK_KEY_MODE_CAPTURE="$KEY_MODE_CAPTURE" \
  MOCK_KEY_HASH_CAPTURE="$KEY_HASH_CAPTURE" \
  MOCK_BUNDLE_FAIL=1 \
  ASC_KEY_ID=ABC123 \
  ASC_ISSUER_ID=issuer-123 \
  ASC_KEY_P8_BASE64="$secret_b64" \
  MATCH_GIT_URL=git@example.invalid:signing.git \
  MATCH_PASSWORD='match-password-secret' \
  bash "$SCRIPT" 2>&1)"
FAIL_RC=$?
failed_key_path="$(cat "$KEY_PATH_CAPTURE" 2>/dev/null || true)"
if [ "$FAIL_RC" -ne 42 ]; then
  bad "fastlane failure should propagate unchanged" "$FAIL_OUTPUT"
elif [ -e "$failed_key_path" ] || [ -d "$(dirname "$failed_key_path")" ]; then
  bad "fastlane failure must still remove temporary key material" "$failed_key_path"
elif ! grep -q '^security <delete-keychain> <gocdnext-' "$CALLS"; then
  bad "fastlane failure must still attempt keychain cleanup" "$(cat "$CALLS")"
fi

if [ "$fails" -ne 0 ]; then
  printf '%d test(s) failed\n' "$fails" >&2
  exit 1
fi
printf 'all signing wrapper tests passed\n'
