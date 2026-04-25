#!/bin/bash
# gocdnext/argocd — thin wrapper around `argocd`. See Dockerfile
# for the input contract.

set -euo pipefail

if [ -z "${PLUGIN_SERVER:-}" ]; then
    echo "gocdnext/argocd: PLUGIN_SERVER is required (e.g. https://argocd.corp)" >&2
    exit 2
fi
if [ -z "${PLUGIN_AUTH_TOKEN:-}" ]; then
    echo "gocdnext/argocd: PLUGIN_AUTH_TOKEN is required" >&2
    echo "  generate via: argocd account generate-token" >&2
    echo "  then pipe through secrets: in your YAML" >&2
    exit 2
fi
if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/argocd: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: app sync api" >&2
    echo "    command: app wait api --health --timeout 600" >&2
    echo "    command: app rollback api 3" >&2
    exit 2
fi

# argocd CLI reads ARGOCD_SERVER + ARGOCD_AUTH_TOKEN from env so
# we don't have to pass --server / --auth-token on every command.
export ARGOCD_SERVER="${PLUGIN_SERVER#https://}"
export ARGOCD_SERVER="${ARGOCD_SERVER#http://}"
export ARGOCD_AUTH_TOKEN="${PLUGIN_AUTH_TOKEN}"

flags=()
if [ "${PLUGIN_INSECURE:-false}" = "true" ]; then
    flags+=("--insecure")
fi
if [ "${PLUGIN_GRPC_WEB:-false}" = "true" ]; then
    flags+=("--grpc-web")
fi

# shellcheck disable=SC2086
exec argocd "${flags[@]}" ${PLUGIN_COMMAND}
