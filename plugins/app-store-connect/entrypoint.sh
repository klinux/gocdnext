#!/bin/bash
# Validates the plugin contract, decodes the App Store Connect .p8 key into a
# mode-0600 file outside the workspace, and runs publish.py as a child so the
# EXIT trap always removes the key. Credentials and release notes stay in env.
set -euo pipefail

PUBLISH="${ASC_PUBLISH:-/opt/app-store-connect/publish.py}"
err() { echo "gocdnext/app-store-connect: $1" >&2; exit 2; }

[ -n "${PLUGIN_APP_ID:-}" ]        || err "app-id is required"
[ -n "${PLUGIN_IPA:-}" ]           || err "ipa is required"
[ -n "${PLUGIN_API_KEY_ID:-}" ]    || err "api-key-id is required (supply from secrets:)"
[ -n "${PLUGIN_API_ISSUER_ID:-}" ] || err "api-issuer-id is required (supply from secrets:)"
[ -n "${PLUGIN_API_KEY:-}" ]       || err "api-key is required (base64 .p8; supply from secrets:)"

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

[ -f "${PLUGIN_IPA}" ] || err "ipa not found: ${PLUGIN_IPA}"
case "${PLUGIN_IPA}" in
    *.ipa) : ;;
    *) err "ipa must end in .ipa (got '${PLUGIN_IPA}')" ;;
esac

track="${PLUGIN_TRACK:-upload}"
case "$track" in
    upload|testflight) : ;;
    *) err "track must be upload|testflight (got '${track}')" ;;
esac
export PLUGIN_TRACK="$track"

if [ "$track" != "testflight" ] && { [ -n "${PLUGIN_BETA_GROUPS:-}" ] || [ -n "${PLUGIN_RELEASE_NOTES:-}" ]; }; then
    err "beta-groups and release-notes require track: testflight"
fi

platform="${PLUGIN_PLATFORM:-IOS}"
case "$platform" in
    IOS|TV_OS|VISION_OS) : ;;
    *) err "platform must be IOS|TV_OS|VISION_OS for an .ipa (got '${platform}')" ;;
esac
export PLUGIN_PLATFORM="$platform"

language="${PLUGIN_LANGUAGE:-en-US}"
if ! [[ "$language" =~ ^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$ ]]; then
    err "language must be a BCP-47-like tag such as en-US or pt-BR (got '${language}')"
fi
export PLUGIN_LANGUAGE="$language"

wait_timeout="${PLUGIN_WAIT_TIMEOUT:-1800}"
if ! [[ "$wait_timeout" =~ ^[0-9]+$ ]] || [ "$wait_timeout" -lt 1 ] || [ "$wait_timeout" -gt 7200 ]; then
    err "wait-timeout must be an integer from 1 to 7200 seconds (got '${wait_timeout}')"
fi
export PLUGIN_WAIT_TIMEOUT="$wait_timeout"

# Force /tmp rather than TMPDIR: the workspace may be mounted elsewhere and
# artifacts/cache must never be able to capture the private key.
CRED_DIR="$(mktemp -d /tmp/gocdnext-asc.XXXXXX)"
trap 'rm -rf "$CRED_DIR"' EXIT
KEY_FILE="${CRED_DIR}/AuthKey.p8"
if ! ( umask 077; printf '%s' "${PLUGIN_API_KEY}" | base64 -d >"$KEY_FILE" ) 2>/dev/null; then
    err "api-key is not valid base64"
fi
[ -s "$KEY_FILE" ] || err "decoded api-key is empty"
chmod 600 "$KEY_FILE"
export ASC_KEY_PATH="$KEY_FILE"

echo "==> uploading ${PLUGIN_IPA} to App Store Connect (track=${track})"
# Not exec: the EXIT trap must remove the key after success or failure.
python3 "$PUBLISH"
