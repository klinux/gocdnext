#!/bin/sh
# gocdnext/vercel entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/vercel: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

[ -n "${VERCEL_TOKEN:-}" ] || fail "VERCEL_TOKEN env is required (secrets: [VERCEL_TOKEN])"
[ -n "${VERCEL_ORG_ID:-}" ] || fail "VERCEL_ORG_ID env is required (the CLI's headless project linkage)"
[ -n "${VERCEL_PROJECT_ID:-}" ] || fail "VERCEL_PROJECT_ID env is required"

# The Vercel CLI does not read VERCEL_TOKEN from the env, and
# --token would put the secret on argv (visible in /proc/ for
# the process lifetime). Instead, write the CLI's own auth.json
# into a private global-config dir — the same file `vercel login`
# produces — and point the CLI at it. argv stays clean.
# (no cleanup trap: the final `exec` replaces the shell, and the
# job container is destroyed after the run — mktemp -d is 0700.)
VCONF="$(mktemp -d)"
node -e 'require("fs").writeFileSync(process.argv[1], JSON.stringify({token: process.env.VERCEL_TOKEN}), {mode: 0o600})' "${VCONF}/auth.json"
set -- deploy --yes --global-config "${VCONF}"
PROD="$(printf '%s' "${PLUGIN_PROD:-false}" | tr '[:upper:]' '[:lower:]')"
[ "${PROD}" = "true" ] && set -- "$@" --prod
PREBUILT="$(printf '%s' "${PLUGIN_PREBUILT:-false}" | tr '[:upper:]' '[:lower:]')"
[ "${PREBUILT}" = "true" ] && set -- "$@" --prebuilt

echo "==> vercel deploy (prod=${PROD} prebuilt=${PREBUILT})"
exec vercel "$@"
