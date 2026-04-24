#!/bin/bash
# gocdnext/kubectl — wraps the kubectl CLI. See Dockerfile for the
# full input contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/kubectl: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: apply -f k8s/" >&2
    echo "    command: rollout status deploy/api -n prod" >&2
    exit 2
fi

# Resolve kubeconfig source when one is provided. Two shapes are
# accepted so operators can pick what fits their secret storage:
#   - a path relative to /workspace (checked-in dev kubeconfig)
#   - a raw YAML blob (likely injected via the job's `secrets:`
#     list for prod credentials)
# Base64 support is intentional: copying a multi-line YAML
# through env vars is brittle; operators who store the config
# as base64 in a secret get a straight decode path.
if [ -n "${PLUGIN_KUBECONFIG:-}" ]; then
    dest=/tmp/gocdnext-kubeconfig
    if [ -f "/workspace/${PLUGIN_KUBECONFIG}" ]; then
        cp "/workspace/${PLUGIN_KUBECONFIG}" "${dest}"
    elif echo "${PLUGIN_KUBECONFIG}" | base64 -d >"${dest}" 2>/dev/null \
         && head -c 7 "${dest}" | grep -q 'apiVersion\|kind:'; then
        : # decoded ok and looks like YAML
    else
        # Treat as literal YAML — covers the inline case.
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
exec kubectl "${ns_args[@]}" ${PLUGIN_COMMAND}
