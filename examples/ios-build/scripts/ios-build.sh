#!/usr/bin/env bash
# Keep App Store Connect key material and the ephemeral keychain lifecycle
# around the complete `bundle exec fastlane build` invocation.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

for name in ASC_KEY_ID ASC_ISSUER_ID ASC_KEY_P8_BASE64 MATCH_GIT_URL MATCH_PASSWORD; do
  [ -n "${!name:-}" ] || die "required environment variable ${name} is empty"
done

ENV_FILE="${GOCDNEXT_IOS_ENV_FILE:-${HOME}/.config/gocdnext/ios-toolchain.env}"
[ -r "$ENV_FILE" ] || die "iOS toolchain environment not found at ${ENV_FILE}"
# shellcheck disable=SC1090
source "$ENV_FILE"
command -v bundle >/dev/null 2>&1 || die "bundle is not available after sourcing ${ENV_FILE}"
command -v openssl >/dev/null 2>&1 || die "openssl is required"

umask 077
TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gocdnext-ios-build.XXXXXX")"
export ASC_KEY_PATH="${TEMP_DIR}/AuthKey_${ASC_KEY_ID}.p8"
export GOCDNEXT_TEMP_KEYCHAIN_NAME="gocdnext-${CI_RUN_ID:-$$}-${RANDOM}.keychain-db"
export GOCDNEXT_TEMP_KEYCHAIN_PASSWORD="$(openssl rand -hex 32)"

cleanup() {
  security delete-keychain "$GOCDNEXT_TEMP_KEYCHAIN_NAME" >/dev/null 2>&1 || true
  rm -rf "$TEMP_DIR"
  unset ASC_KEY_PATH GOCDNEXT_TEMP_KEYCHAIN_NAME GOCDNEXT_TEMP_KEYCHAIN_PASSWORD
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if ! printf '%s' "$ASC_KEY_P8_BASE64" | base64 --decode >"$ASC_KEY_PATH" 2>/dev/null; then
  printf '%s' "$ASC_KEY_P8_BASE64" | base64 -D >"$ASC_KEY_PATH" 2>/dev/null || \
    die "ASC_KEY_P8_BASE64 is not valid base64"
fi
chmod 600 "$ASC_KEY_PATH"
[ -s "$ASC_KEY_PATH" ] || die "decoded App Store Connect key is empty"

# The user-facing convention remains Fastlane: this is the only build command.
# It stays a child so the EXIT trap can clean key material afterward.
bundle exec fastlane build
