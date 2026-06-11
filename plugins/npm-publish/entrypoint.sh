#!/bin/sh
# gocdnext/npm-publish entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/npm-publish: $1" >&2; exit 2; }

DRY_RUN="$(printf '%s' "${PLUGIN_DRY_RUN:-false}" | tr '[:upper:]' '[:lower:]')"
IF_EXISTS="$(printf '%s' "${PLUGIN_IF_EXISTS:-fail}" | tr '[:upper:]' '[:lower:]')"
case "${IF_EXISTS}" in fail|skip) ;; *) fail "if-exists must be fail | skip (got '${PLUGIN_IF_EXISTS}')";; esac

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi
DIR="${PLUGIN_DIR:-.}"
[ -f "${DIR}/package.json" ] || fail "no package.json in '${DIR}'"

REGISTRY="${PLUGIN_REGISTRY:-https://registry.npmjs.org}"
REGISTRY="${REGISTRY%/}"

NAME=$(jq -r '.name // empty' "${DIR}/package.json")
VERSION=$(jq -r '.version // empty' "${DIR}/package.json")
[ -n "${NAME}" ] && [ -n "${VERSION}" ] || fail "package.json must carry name + version"

# Token: required for real publishes; a dry-run packs without auth.
if [ "${DRY_RUN}" != "true" ]; then
    [ -n "${NPM_TOKEN:-}" ] || fail "NPM_TOKEN env is required (secrets: [NPM_TOKEN])"
    # User-scoped .npmrc — the token never touches argv or the
    # workspace (a workspace .npmrc could be archived by artifacts).
    REG_HOST_PATH="${REGISTRY#*//}"
    printf '//%s/:_authToken=%s\n' "${REG_HOST_PATH}" "${NPM_TOKEN}" > "${HOME}/.npmrc"
fi

# Idempotency: name@version is immutable on npm — a retried
# pipeline that already published must not fail the whole run.
if [ "${IF_EXISTS}" = "skip" ] && [ "${DRY_RUN}" != "true" ]; then
    if npm view "${NAME}@${VERSION}" version --registry "${REGISTRY}" >/dev/null 2>&1; then
        echo "==> ${NAME}@${VERSION} already published — if-exists: skip, nothing to do"
        exit 0
    fi
fi

set -- publish --registry "${REGISTRY}"
[ -n "${PLUGIN_TAG:-}" ] && set -- "$@" --tag "${PLUGIN_TAG}"
[ -n "${PLUGIN_ACCESS:-}" ] && set -- "$@" --access "${PLUGIN_ACCESS}"
[ "${DRY_RUN}" = "true" ] && set -- "$@" --dry-run

echo "==> npm publish ${NAME}@${VERSION} (registry=${REGISTRY}${PLUGIN_TAG:+ tag=${PLUGIN_TAG}}${DRY_RUN:+ dry-run=${DRY_RUN}})"
cd "${DIR}"
exec npm "$@"
