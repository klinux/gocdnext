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

# Argo CD config-management-plugin env (e.g. a Helm CMP that reads
# HELM_ARGS). Appended as ONE argument so a multi-token value with
# spaces — "HELM_ARGS=--set image.tag=X -f values.yaml" — survives
# intact. Folding it into PLUGIN_COMMAND would word-split it and Argo
# would see only the first token; this is why a CMP `app set` needs a
# dedicated input rather than the free-form command.
extra=()
if [ -n "${PLUGIN_PLUGIN_ENV:-}" ]; then
    # Light shape guard: a single NAME=value with no newline. Argo's
    # --plugin-env is one assignment; a newline would smuggle a second
    # arg / line into the CLI invocation. NAME must be a real env-ident
    # (the case-glob form accepted "HELM-ARGS=x" / "A B=x").
    case "${PLUGIN_PLUGIN_ENV}" in
        *$'\n'*) echo "gocdnext/argocd: plugin_env must not contain a newline" >&2; exit 2 ;;
    esac
    if ! [[ "${PLUGIN_PLUGIN_ENV}" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
        echo "gocdnext/argocd: plugin_env must be NAME=value (got: ${PLUGIN_PLUGIN_ENV})" >&2
        exit 2
    fi
    extra+=("--plugin-env" "${PLUGIN_PLUGIN_ENV}")
fi

# PLUGIN_COMMAND is intentionally unquoted (word-split into argv); the
# plugin-env value above is the one piece that must NOT be split.
# shellcheck disable=SC2086
exec argocd "${flags[@]}" ${PLUGIN_COMMAND} "${extra[@]}"
