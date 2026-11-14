#!/usr/bin/env bash
# Materialize the ASC key outside the workspace and run fastlane match without
# putting any secret in argv. Intended to be committed alongside the Fastfile.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

for name in ASC_KEY_ID ASC_ISSUER_ID ASC_KEY_P8_BASE64 MATCH_GIT_URL MATCH_PASSWORD; do
  [ -n "${!name:-}" ] || die "required environment variable ${name} is empty"
done

ENV_FILE="${GOCDNEXT_IOS_ENV_FILE:-${HOME}/.config/gocdnext/ios-toolchain.env}"
[ -r "$ENV_FILE" ] || die "iOS toolchain environment not found at ${ENV_FILE}; run bootstrap-ios.sh"
# shellcheck disable=SC1090
source "$ENV_FILE"
command -v bundle >/dev/null 2>&1 || die "bundle is not available after sourcing ${ENV_FILE}"
command -v openssl >/dev/null 2>&1 || die "openssl is required to generate an ephemeral keychain password"

umask 077
TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gocdnext-ios-signing.XXXXXX")"
export ASC_KEY_PATH="${TEMP_DIR}/AuthKey_${ASC_KEY_ID}.p8"
export GOCDNEXT_TEMP_KEYCHAIN_NAME="gocdnext-${CI_RUN_ID:-$$}-${RANDOM}.keychain-db"
export GOCDNEXT_TEMP_KEYCHAIN_PASSWORD="$(openssl rand -hex 32)"

cleanup() {
  # Fastfile's ensure block is the primary cleanup. This catches interruption
  # after the keychain was created but before fastlane's ensure can complete.
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

# Run as a child rather than exec so the EXIT trap removes the key material.
bundle exec fastlane ios sync_signing
