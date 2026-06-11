#!/bin/sh
# gocdnext/netlify entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/netlify: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

DIR="${PLUGIN_DIR:-}"
[ -n "${DIR}" ] || fail "dir: is required (the built site directory)"
[ -d "${DIR}" ] || fail "dir '${DIR}' not found in the workspace"
[ -n "${NETLIFY_AUTH_TOKEN:-}" ] || fail "NETLIFY_AUTH_TOKEN env is required (secrets: [NETLIFY_AUTH_TOKEN])"

SITE_ID="${PLUGIN_SITE_ID:-${NETLIFY_SITE_ID:-}}"
[ -n "${SITE_ID}" ] || fail "site-id: is required (or NETLIFY_SITE_ID via secrets)"
export NETLIFY_SITE_ID="${SITE_ID}"

set -- deploy --dir "${DIR}" --no-build
PROD="$(printf '%s' "${PLUGIN_PROD:-false}" | tr '[:upper:]' '[:lower:]')"
[ "${PROD}" = "true" ] && set -- "$@" --prod
[ -n "${PLUGIN_ALIAS:-}" ] && set -- "$@" --alias "${PLUGIN_ALIAS}"

echo "==> netlify deploy (dir=${DIR} site=${SITE_ID} prod=${PROD}${PLUGIN_ALIAS:+ alias=${PLUGIN_ALIAS}})"
# Token + site id ride env (the CLI's native contract) — argv
# carries only paths and flags.
exec netlify "$@"
