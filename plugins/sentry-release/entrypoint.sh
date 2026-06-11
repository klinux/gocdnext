#!/bin/sh
# gocdnext/sentry-release entrypoint — see Dockerfile.

set -eu

fail() { echo "gocdnext/sentry-release: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

[ -n "${SENTRY_AUTH_TOKEN:-}" ] || fail "SENTRY_AUTH_TOKEN env is required (secrets: [SENTRY_AUTH_TOKEN])"
ORG="${PLUGIN_ORG:-${SENTRY_ORG:-}}"
PROJECT="${PLUGIN_PROJECT:-${SENTRY_PROJECT:-}}"
[ -n "${ORG}" ] || fail "org: is required (or SENTRY_ORG env)"
[ -n "${PROJECT}" ] || fail "project: is required (or SENTRY_PROJECT env)"
export SENTRY_ORG="${ORG}" SENTRY_PROJECT="${PROJECT}"

# Version precedence: explicit input → the git tag that triggered
# the run → the commit SHA. Releases keyed by SHA still correlate
# errors↔deploys; tags read nicer in the Sentry UI.
VERSION="${PLUGIN_VERSION:-${CI_TAG_NAME:-${CI_COMMIT_SHA:-}}}"
[ -n "${VERSION}" ] || fail "version: is required when the run carries neither CI_TAG_NAME nor CI_COMMIT_SHA"

echo "==> sentry release ${VERSION} (org=${ORG} project=${PROJECT})"
sentry-cli releases new "${VERSION}"

SET_COMMITS="$(printf '%s' "${PLUGIN_SET_COMMITS:-true}" | tr '[:upper:]' '[:lower:]')"
if [ "${SET_COMMITS}" = "true" ]; then
    git config --global --add safe.directory '*' 2>/dev/null || true
    # --ignore-missing: a shallow clone can't always resolve the
    # previous release's commit — degrade to a commit-less release
    # instead of failing the deploy pipeline over metadata.
    sentry-cli releases set-commits "${VERSION}" --auto --ignore-missing || \
        echo "    set-commits skipped (no repo mapping in Sentry or unresolvable range)"
fi

if [ -n "${PLUGIN_SOURCEMAPS:-}" ]; then
    [ -d "${PLUGIN_SOURCEMAPS}" ] || fail "sourcemaps dir '${PLUGIN_SOURCEMAPS}' not found"
    echo "    uploading sourcemaps from ${PLUGIN_SOURCEMAPS}"
    sentry-cli sourcemaps upload --release "${VERSION}" "${PLUGIN_SOURCEMAPS}"
fi

FINALIZE="$(printf '%s' "${PLUGIN_FINALIZE:-true}" | tr '[:upper:]' '[:lower:]')"
if [ "${FINALIZE}" = "true" ]; then
    sentry-cli releases finalize "${VERSION}"
    echo "    finalized"
fi

if [ -n "${PLUGIN_ENVIRONMENT:-}" ]; then
    sentry-cli releases deploys "${VERSION}" new -e "${PLUGIN_ENVIRONMENT}"
    echo "    deploy marked (env=${PLUGIN_ENVIRONMENT})"
fi
