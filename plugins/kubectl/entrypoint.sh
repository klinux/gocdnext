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
    if [ -f "${PLUGIN_KUBECONFIG}" ]; then
        cp "${PLUGIN_KUBECONFIG}" "${dest}"
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

# continue_on_error: a DIAGNOSTIC command must not fail the run just because
# it errors. The load-bearing case (#303): a `logs` step that tails a
# migration Job's pod AFTER a separate `wait --for=condition=complete` already
# confirmed the Job finished — if the target-cluster SA lacks `pods/log`, the
# `kubectl logs` returns Forbidden and (without this) fails the run even though
# the migration already ran, so operators re-run and double-mutate. When set,
# a non-zero exit is downgraded to a WARNING and the step succeeds. OFF by
# default: apply/rollout/wait failures must still fail the run.
continue_on_error=0
case "$(printf '%s' "${PLUGIN_CONTINUE_ON_ERROR:-}" | tr '[:upper:]' '[:lower:]')" in
    true | 1 | yes | on) continue_on_error=1 ;;
esac

if [ "${continue_on_error}" = 1 ]; then
    # shellcheck disable=SC2086
    kubectl "${ns_args[@]}" ${PLUGIN_COMMAND} && rc=0 || rc=$?
    if [ "${rc}" -ne 0 ]; then
        echo "gocdnext/kubectl: command exited ${rc}, but continue_on_error is set — NOT failing the run." >&2
        echo "  command: kubectl ${ns_args[*]} ${PLUGIN_COMMAND}" >&2
        echo "  (if this was 'logs' with 'pods/log ... Forbidden', grant pods/log to the deployer SA — see docs: concepts/kubernetes-runtime, RBAC.)" >&2
    fi
    exit 0
fi

# shellcheck disable=SC2086
exec kubectl "${ns_args[@]}" ${PLUGIN_COMMAND}
