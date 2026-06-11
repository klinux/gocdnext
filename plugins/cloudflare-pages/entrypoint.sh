#!/bin/sh
# gocdnext/cloudflare-pages entrypoint — see Dockerfile.

set -eu

fail() { echo "gocdnext/cloudflare-pages: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

DIR="${PLUGIN_DIR:-}"
[ -n "${DIR}" ] || fail "dir: is required (the built site directory)"
[ -d "${DIR}" ] || fail "dir '${DIR}' not found in the workspace"
PROJECT="${PLUGIN_PROJECT_NAME:-}"
[ -n "${PROJECT}" ] || fail "project-name: is required"
[ -n "${CLOUDFLARE_API_TOKEN:-}" ] || fail "CLOUDFLARE_API_TOKEN env is required (secrets: [CLOUDFLARE_API_TOKEN])"
[ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ] || fail "CLOUDFLARE_ACCOUNT_ID env is required"

set -- pages deploy "${DIR}" --project-name "${PROJECT}"
[ -n "${PLUGIN_BRANCH:-}" ] && set -- "$@" --branch "${PLUGIN_BRANCH}"

echo "==> wrangler pages deploy (dir=${DIR} project=${PROJECT}${PLUGIN_BRANCH:+ branch=${PLUGIN_BRANCH}})"
# Token + account ride env (wrangler's native contract) — argv
# carries only paths and the project name.
exec wrangler "$@"
