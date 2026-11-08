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

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Kubeconfig discovery — mirrors the kubectl plugin verbatim so
# operators don't have to remember two conventions. See comments
# in plugins/kubectl/entrypoint.sh for the path/inline/base64
# rationale.
if [ -n "${PLUGIN_KUBECONFIG:-}" ]; then
    # The config holds tokens/certs. Materialise into a FRESH, unique, 0600 file
    # anchored to /tmp (a bare `mktemp` honours $TMPDIR, which a job could point at
    # a persisted /workspace volume; a fixed path could preexist permissive and its
    # mode would be inherited by the write). Write with REDIRECTION — `cp` would
    # copy the source file's mode. This image has no kubectl, so base64-vs-literal
    # is decided by scanning the WHOLE decoded file for `apiVersion` (not a fixed
    # byte window that misses keys behind a comment); `helm` itself is the ultimate
    # validator when it runs.
    dest="$(mktemp /tmp/gocdnext-kubeconfig.XXXXXX)"
    if [ -f "${PLUGIN_KUBECONFIG}" ]; then
        cat "${PLUGIN_KUBECONFIG}" >"${dest}"
    elif echo "${PLUGIN_KUBECONFIG}" | base64 -d >"${dest}" 2>/dev/null \
         && grep -qa 'apiVersion' "${dest}"; then
        :
    else
        printf '%s' "${PLUGIN_KUBECONFIG}" >"${dest}"
    fi
    export KUBECONFIG="${dest}"
fi

ns_args=()
if [ -n "${PLUGIN_NAMESPACE:-}" ]; then
    ns_args+=("--namespace" "${PLUGIN_NAMESPACE}")
fi

# Word-split WITHOUT glob expansion: a bare, unquoted ${PLUGIN_COMMAND} would also
# expand `*`, `?`, `[...]` against the workspace, silently rewriting args (e.g. a
# `--set` value or a path) from whatever files happen to be present. read -a splits
# on IFS only. (Whitespace inside a single arg still isn't supported — the contract
# says drop to a plain `script:` for that.)
read -r -a command_args <<< "${PLUGIN_COMMAND}"
exec helm "${ns_args[@]}" "${command_args[@]}"
