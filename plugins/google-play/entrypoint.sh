#!/bin/bash
# gocdnext/google-play — publish a signed Android artifact to Google Play.
#
# Validates inputs, materialises the service-account credentials into a 0600
# file OUTSIDE the workspace (so they are never captured by artifacts:/cache:),
# points GOOGLE_APPLICATION_CREDENTIALS at it, and hands off to publish.py which
# runs the Play Developer API edit transaction. The credential JSON arrives via
# the PLUGIN_SERVICE_ACCOUNT_JSON env var (from secrets:) and NEVER reaches argv.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PACKAGE_NAME        (required) applicationId, e.g. com.example.app.
#   PLUGIN_ARTIFACT            (required) path to the signed .aab or .apk.
#   PLUGIN_TRACK               (required) internal|alpha|beta|production (or a
#                                         custom closed-testing track name).
#   PLUGIN_SERVICE_ACCOUNT_JSON(required) SA key JSON (from secrets:).
#   PLUGIN_STATUS              (optional) completed|draft|inProgress|halted;
#                                         default completed.
#   PLUGIN_ROLLOUT             (req. if status inProgress/halted) staged fraction
#                                         in (0,1), e.g. 0.1 (100% = status completed).
#   PLUGIN_RELEASE_NOTES       (optional) release notes text.
#   PLUGIN_LANGUAGE            (optional) BCP-47 lang for the notes; default en-US.
#   PLUGIN_MAPPING             (optional) path to R8/ProGuard mapping.txt to
#                                         upload for crash de-obfuscation.
set -euo pipefail

PUBLISH="${GPLAY_PUBLISH:-/opt/gplay/publish.py}"

err() { echo "gocdnext/google-play: $1" >&2; exit 2; }

[ -n "${PLUGIN_PACKAGE_NAME:-}" ] || err "package-name is required"
[ -n "${PLUGIN_ARTIFACT:-}" ]     || err "artifact is required"
[ -n "${PLUGIN_TRACK:-}" ]        || err "track is required"
[ -n "${PLUGIN_SERVICE_ACCOUNT_JSON:-}" ] || err "service-account-json is required (supply from secrets:)"

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

[ -f "${PLUGIN_ARTIFACT}" ] || err "artifact not found: ${PLUGIN_ARTIFACT}"
case "${PLUGIN_ARTIFACT}" in
    *.aab|*.apk) : ;;
    *) err "artifact must be a .aab or .apk (got '${PLUGIN_ARTIFACT}')" ;;
esac

status="${PLUGIN_STATUS:-completed}"
case "$status" in
    completed|draft|inProgress|halted) : ;;
    *) err "status must be completed|draft|inProgress|halted (got '${status}')" ;;
esac
export PLUGIN_STATUS="$status"

if [ "$status" = "inProgress" ] || [ "$status" = "halted" ]; then
    [ -n "${PLUGIN_ROLLOUT:-}" ] || err "rollout is required when status is '${status}' (a fraction in (0,1) — use status: completed for 100%)"
    # Numeric + range check in awk (handles the float; bash can't). The Play API
    # requires 0 < userFraction < 1 for a staged rollout; a full 100% release is
    # status: completed, NOT rollout 1 — so reject >= 1 here instead of letting
    # tracks.update fail remotely.
    if ! awk -v r="${PLUGIN_ROLLOUT}" 'BEGIN{ if (r+0!=r || r<=0 || r>=1) exit 1 }' 2>/dev/null; then
        err "rollout must be a number in (0,1) (got '${PLUGIN_ROLLOUT}') — use status: completed for a full rollout"
    fi
fi

if [ -n "${PLUGIN_MAPPING:-}" ] && [ ! -f "${PLUGIN_MAPPING}" ]; then
    err "mapping not found: ${PLUGIN_MAPPING}"
fi

# Materialise SA credentials into a private tempfile outside the workspace,
# cleaned on exit. The JSON is piped from the env var — never on argv.
CRED_DIR="$(mktemp -d)"
trap 'rm -rf "$CRED_DIR"' EXIT
CRED_FILE="${CRED_DIR}/sa.json"
( umask 077; printf '%s' "${PLUGIN_SERVICE_ACCOUNT_JSON}" > "$CRED_FILE" )
[ -s "$CRED_FILE" ] || err "service-account-json decoded empty"
export GOOGLE_APPLICATION_CREDENTIALS="$CRED_FILE"

echo "==> publishing ${PLUGIN_ARTIFACT} to track '${PLUGIN_TRACK}' (status=${status})"
# Not `exec`: run python3 as a child so the EXIT trap still fires and removes the
# credential tempfile. set -e propagates a publish failure (trap runs either way).
python3 "$PUBLISH"
