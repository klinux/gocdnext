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

# Keep the token off BOTH argv and the deploy process env. `--token` would put
# the secret on argv (visible in /proc for the process lifetime); and while the
# Vercel CLI v65 DOES read VERCEL_TOKEN from the env, leaving it there exposes it
# to everything the deploy spawns. So write the CLI's own auth.json (the file
# `vercel login` produces) into a private 0700 dir, point the CLI at it with
# --global-config, and UNSET VERCEL_TOKEN so the CLI authenticates from the file,
# not the env. (no cleanup trap: the final `exec` replaces the shell, and the job
# container is destroyed after the run.)
VCONF="$(mktemp -d)"
node -e 'require("fs").writeFileSync(process.argv[1], JSON.stringify({token: process.env.VERCEL_TOKEN}), {mode: 0o600})' "${VCONF}/auth.json"
unset VERCEL_TOKEN
set -- deploy --yes --global-config "${VCONF}"
PROD="$(printf '%s' "${PLUGIN_PROD:-false}" | tr '[:upper:]' '[:lower:]')"
[ "${PROD}" = "true" ] && set -- "$@" --prod
PREBUILT="$(printf '%s' "${PLUGIN_PREBUILT:-false}" | tr '[:upper:]' '[:lower:]')"
[ "${PREBUILT}" = "true" ] && set -- "$@" --prebuilt

echo "==> vercel deploy (prod=${PROD} prebuilt=${PREBUILT})"
exec vercel "$@"
