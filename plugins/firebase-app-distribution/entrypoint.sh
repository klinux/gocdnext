#!/bin/bash
# gocdnext/firebase-app-distribution — distribute an Android build to testers via
# Firebase App Distribution. Pairs with the `android` plugin: point `artifact`
# at the APK/AAB it produced.
#
# Uses the Firebase CLI (`firebase appdistribution:distribute`) authenticated
# with a service account. The SA JSON (from secrets:) is written to a 0600
# tempfile outside the workspace and exposed only via
# GOOGLE_APPLICATION_CREDENTIALS — never on argv. Release notes go through a
# temp file (--release-notes-file) so arbitrary/multiline text never hits argv.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_APP_ID              (required) Firebase App ID, e.g.
#                                         1:1234567890:android:abcdef.
#   PLUGIN_ARTIFACT            (required) path to the .apk or .aab.
#   PLUGIN_SERVICE_ACCOUNT_JSON(required) SA key JSON (from secrets:).
#   PLUGIN_GROUPS              (one of groups/testers required) comma-separated
#                                         tester GROUP aliases.
#   PLUGIN_TESTERS             (one of groups/testers required) comma-separated
#                                         tester EMAILS.
#   PLUGIN_RELEASE_NOTES       (optional) release notes text.
set -euo pipefail

FIREBASE_BIN="${FIREBASE_BIN:-firebase}"
err() { echo "gocdnext/firebase-app-distribution: $1" >&2; exit 2; }

[ -n "${PLUGIN_APP_ID:-}" ]   || err "app-id is required"
[ -n "${PLUGIN_ARTIFACT:-}" ] || err "artifact is required"
[ -n "${PLUGIN_SERVICE_ACCOUNT_JSON:-}" ] || err "service-account-json is required (supply from secrets:)"
[ -n "${PLUGIN_GROUPS:-}" ] || [ -n "${PLUGIN_TESTERS:-}" ] || err "at least one of groups or testers is required"

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

[ -f "${PLUGIN_ARTIFACT}" ] || err "artifact not found: ${PLUGIN_ARTIFACT}"
case "${PLUGIN_ARTIFACT}" in
    *.aab|*.apk) : ;;
    *) err "artifact must be a .aab or .apk (got '${PLUGIN_ARTIFACT}')" ;;
esac

# Credentials + release-notes into a private tempdir outside the workspace,
# removed on exit.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
CRED="${WORK}/sa.json"
( umask 077; printf '%s' "${PLUGIN_SERVICE_ACCOUNT_JSON}" > "$CRED" )
[ -s "$CRED" ] || err "service-account-json decoded empty"
export GOOGLE_APPLICATION_CREDENTIALS="$CRED"

# Build argv positionally so the artifact path + list flags are passed safely.
set -- "${PLUGIN_ARTIFACT}" --app "${PLUGIN_APP_ID}"
[ -n "${PLUGIN_GROUPS:-}" ]  && set -- "$@" --groups "${PLUGIN_GROUPS}"
[ -n "${PLUGIN_TESTERS:-}" ] && set -- "$@" --testers "${PLUGIN_TESTERS}"
if [ -n "${PLUGIN_RELEASE_NOTES:-}" ]; then
    NOTES="${WORK}/notes.txt"
    printf '%s' "${PLUGIN_RELEASE_NOTES}" > "$NOTES"
    set -- "$@" --release-notes-file "$NOTES"
fi

echo "==> firebase appdistribution:distribute (app ${PLUGIN_APP_ID})"
# Child process (not exec) so the EXIT trap removes the credentials afterward.
"$FIREBASE_BIN" appdistribution:distribute "$@"
