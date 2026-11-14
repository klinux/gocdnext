#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="${HERE}/ios-build.sh"
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
[ "${MOCK_BUNDLE_FAIL:-0}" != "1" ] || exit 37
STUB

cat >"${BIN}/openssl" <<'STUB'
#!/usr/bin/env bash
printf 'ephemeral-password-not-logged\n'
STUB

cat >"${BIN}/security" <<'STUB'
#!/usr/bin/env bash
printf 'security' >>"$MOCK_CALLS"
printf ' <%s>' "$@" >>"$MOCK_CALLS"
printf '\n' >>"$MOCK_CALLS"
STUB
chmod +x "${BIN}"/*

secret_p8='-----BEGIN PRIVATE KEY-----
ios-build-secret
-----END PRIVATE KEY-----'
secret_b64="$(printf '%s' "$secret_p8" | base64 -w 0)"

run_build() {
  local fail="${1:-0}"
  OUTPUT="$(env \
    PATH="${BIN}:/usr/bin:/bin" \
    HOME="${TMP}/home" \
    TMPDIR="$TMP" \
    MOCK_CALLS="$CALLS" \
    MOCK_KEY_PATH_CAPTURE="$KEY_PATH_CAPTURE" \
    MOCK_KEY_MODE_CAPTURE="$KEY_MODE_CAPTURE" \
    MOCK_KEY_HASH_CAPTURE="$KEY_HASH_CAPTURE" \
    MOCK_BUNDLE_FAIL="$fail" \
    ASC_KEY_ID=ABC123 \
    ASC_ISSUER_ID=issuer-123 \
    ASC_KEY_P8_BASE64="$secret_b64" \
    MATCH_GIT_URL=git@example.invalid:signing.git \
    MATCH_PASSWORD='match-secret' \
    bash "$SCRIPT" 2>&1)"
  RC=$?
}

fails=0
bad() { printf 'FAIL: %s\n%s\n' "$1" "${2:-}"; fails=$((fails + 1)); }

run_build 0
calls="$(cat "$CALLS")"
key_path="$(cat "$KEY_PATH_CAPTURE" 2>/dev/null || true)"
want_hash="$(printf '%s' "$secret_p8" | sha256sum | awk '{ print $1 }')"
if [ "$RC" -ne 0 ]; then
  bad "build wrapper should succeed" "$OUTPUT"
fi
if ! grep -q '^bundle <exec> <fastlane> <build>$' <<<"$calls"; then
  bad "build argv must be exactly bundle exec fastlane build" "$calls"
fi
if [ "$(cat "$KEY_MODE_CAPTURE" 2>/dev/null || true)" != "600" ] || \
   [ "$(cat "$KEY_HASH_CAPTURE" 2>/dev/null || true)" != "$want_hash" ]; then
  bad "wrapper must materialize the exact .p8 as mode 0600"
fi
if [ -e "$key_path" ] || [ -d "$(dirname "$key_path")" ]; then
  bad "successful build must remove temporary key material" "$key_path"
fi
if grep -Fq 'ios-build-secret' <<<"${OUTPUT}${calls}" || \
   grep -Fq 'match-secret' <<<"${OUTPUT}${calls}" || \
   grep -Fq "$secret_b64" <<<"${OUTPUT}${calls}"; then
  bad "signing secret leaked to output or argv" "${OUTPUT}${calls}"
fi

: >"$CALLS"
rm -f "$KEY_PATH_CAPTURE" "$KEY_MODE_CAPTURE" "$KEY_HASH_CAPTURE"
run_build 1
failed_key_path="$(cat "$KEY_PATH_CAPTURE" 2>/dev/null || true)"
if [ "$RC" -ne 37 ]; then
  bad "fastlane failure should propagate" "$OUTPUT"
elif [ -e "$failed_key_path" ] || [ -d "$(dirname "$failed_key_path")" ]; then
  bad "failed build must remove temporary key material" "$failed_key_path"
elif ! grep -q '^security <delete-keychain> <gocdnext-' "$CALLS"; then
  bad "failed build must attempt keychain cleanup" "$(cat "$CALLS")"
fi

if [ "$fails" -ne 0 ]; then
  printf '%d test(s) failed\n' "$fails" >&2
  exit 1
fi
printf 'all iOS build wrapper tests passed\n'
