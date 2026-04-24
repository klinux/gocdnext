#!/bin/bash
# gocdnext/helm — thin wrapper around `helm`. See Dockerfile for
# the full input contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/helm: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: upgrade --install api ./chart -n prod" >&2
    echo "    command: diff upgrade api ./chart" >&2
    exit 2
fi

cd /workspace
if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Kubeconfig discovery — mirrors the kubectl plugin verbatim so
# operators don't have to remember two conventions. See comments
# in plugins/kubectl/entrypoint.sh for the path/inline/base64
# rationale.
if [ -n "${PLUGIN_KUBECONFIG:-}" ]; then
    dest=/tmp/gocdnext-kubeconfig
    if [ -f "/workspace/${PLUGIN_KUBECONFIG}" ]; then
        cp "/workspace/${PLUGIN_KUBECONFIG}" "${dest}"
    elif echo "${PLUGIN_KUBECONFIG}" | base64 -d >"${dest}" 2>/dev/null \
         && head -c 7 "${dest}" | grep -q 'apiVersion\|kind:'; then
        :
    else
        printf '%s' "${PLUGIN_KUBECONFIG}" >"${dest}"
    fi
    chmod 0600 "${dest}"
    export KUBECONFIG="${dest}"
fi

ns_args=()
if [ -n "${PLUGIN_NAMESPACE:-}" ]; then
    ns_args+=("--namespace" "${PLUGIN_NAMESPACE}")
fi

# shellcheck disable=SC2086
exec helm "${ns_args[@]}" ${PLUGIN_COMMAND}
