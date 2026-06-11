#!/bin/sh
# gocdnext/maven-central-publish entrypoint — see Dockerfile.
#
# Central Portal API (https://central.sonatype.org/publish/publish-portal-api/):
#   POST /api/v1/publisher/upload          multipart bundle → deployment id
#   POST /api/v1/publisher/status?id=...   PENDING → VALIDATING →
#                                          VALIDATED → PUBLISHING → PUBLISHED
#                                          (or FAILED with per-file errors)

set -eu

fail() { echo "gocdnext/maven-central-publish: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

BUNDLE="${PLUGIN_BUNDLE:-}"
[ -n "${BUNDLE}" ] || fail "bundle: is required (path to the deployment zip — jars, poms, .asc, checksums in Central layout)"
[ -f "${BUNDLE}" ] || fail "bundle '${BUNDLE}' not found in the workspace"
[ -n "${CENTRAL_TOKEN:-}" ] || fail "CENTRAL_TOKEN env is required (secrets: [CENTRAL_TOKEN] — portal token from central.sonatype.com)"

API="${PLUGIN_API_BASE:-https://central.sonatype.com}"
TYPE="$(printf '%s' "${PLUGIN_PUBLISHING_TYPE:-AUTOMATIC}" | tr '[:lower:]' '[:upper:]')"
case "${TYPE}" in AUTOMATIC|USER_MANAGED) ;; *) fail "publishing-type must be AUTOMATIC | USER_MANAGED (got '${TYPE}')";; esac

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
printf 'header = "Authorization: Bearer %s"\n' "${CENTRAL_TOKEN}" > "${WORK}/auth"

echo "==> uploading $(basename "${BUNDLE}") ($(wc -c < "${BUNDLE}") bytes, publishingType=${TYPE})"
HTTP=$(curl --silent --show-error --config "${WORK}/auth" \
    --write-out '%{http_code}' --output "${WORK}/resp" \
    --form "bundle=@${BUNDLE}" \
    "${API}/api/v1/publisher/upload?publishingType=${TYPE}") || fail "upload request failed"
if [ "${HTTP}" -ge 400 ]; then
    cat "${WORK}/resp" >&2
    fail "portal upload returned HTTP ${HTTP}"
fi
DEPLOYMENT_ID=$(cat "${WORK}/resp")
echo "    deployment id: ${DEPLOYMENT_ID}"

WAIT="$(printf '%s' "${PLUGIN_WAIT:-true}" | tr '[:upper:]' '[:lower:]')"
if [ "${WAIT}" != "true" ]; then
    echo "    wait: false — not polling; check the portal UI for validation status"
    exit 0
fi

# Poll until a terminal state. Validation typically lands in well
# under 10 minutes; the 30-min cap fails loud rather than letting
# a wedged validation look like a hung-but-fine job.
i=0
while [ "${i}" -lt 180 ]; do
    sleep 10
    HTTP=$(curl --silent --show-error --config "${WORK}/auth" \
        --write-out '%{http_code}' --output "${WORK}/status" \
        --request POST \
        "${API}/api/v1/publisher/status?id=${DEPLOYMENT_ID}") || fail "status request failed"
    [ "${HTTP}" -ge 400 ] && { cat "${WORK}/status" >&2; fail "portal status returned HTTP ${HTTP}"; }
    STATE=$(jq -r '.deploymentState // empty' "${WORK}/status")
    echo "    state: ${STATE}"
    case "${STATE}" in
        PUBLISHED) exit 0 ;;
        VALIDATED)
            # Terminal for USER_MANAGED (a human releases via the
            # portal UI); transient for AUTOMATIC.
            [ "${TYPE}" = "USER_MANAGED" ] && { echo "    validated — release manually in the portal"; exit 0; }
            ;;
        FAILED)
            jq -r '.errors // {} | tostring' "${WORK}/status" >&2
            fail "deployment validation FAILED — see errors above"
            ;;
    esac
    i=$((i + 1))
done
fail "timed out after 30min waiting for the deployment to publish"
