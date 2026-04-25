#!/bin/bash
# gocdnext/kustomize — thin wrapper around kustomize + kubectl.
# See Dockerfile for the full input contract.

set -euo pipefail

if [ -z "${PLUGIN_PATH:-}" ]; then
    echo "gocdnext/kustomize: PLUGIN_PATH is required" >&2
    echo "  Example: path: deploy/overlays/prod" >&2
    exit 2
fi

action="${PLUGIN_ACTION:-apply}"
case "${action}" in
    apply|build|diff) ;;
    *)
        echo "gocdnext/kustomize: unknown action '${action}' (use apply|build|diff)" >&2
        exit 2
        ;;
esac

cd /workspace
if [ ! -d "${PLUGIN_PATH}" ]; then
    echo "gocdnext/kustomize: path '${PLUGIN_PATH}' is not a directory under /workspace" >&2
    exit 2
fi

# Kubeconfig discovery — mirrors the kubectl + helm plugins
# verbatim so operators don't juggle three conventions. See
# plugins/kubectl/entrypoint.sh for the rationale on each branch.
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

# Render once, fan out per action. Stderr goes to the job log
# directly; stdout is captured for apply/diff and re-emitted for
# build so operators see what got rendered before an apply.
manifests="$(kustomize build "${PLUGIN_PATH}")"

case "${action}" in
    build)
        printf '%s\n' "${manifests}"
        ;;
    diff)
        printf '%s\n' "${manifests}" \
            | kubectl diff "${ns_args[@]}" -f - || diff_rc=$?
        # kubectl diff exits 1 when there are differences — that's
        # not an error in the gocdnext sense, the job should
        # succeed and surface the diff. Only exit nonzero on
        # genuine kubectl failure (≥2).
        if [ "${diff_rc:-0}" -ge 2 ]; then
            exit "${diff_rc}"
        fi
        ;;
    apply)
        # Echo manifests upfront so the deploy log shows what's
        # being applied — invaluable when debugging a "but it
        # rendered fine locally" mystery.
        echo "--- rendered manifests ---"
        printf '%s\n' "${manifests}"
        echo "--- applying ---"

        prune_args=()
        if [ "${PLUGIN_PRUNE:-false}" = "true" ]; then
            if [ -z "${PLUGIN_PRUNE_LABEL:-}" ]; then
                echo "gocdnext/kustomize: prune=true requires prune_label (e.g. app.kubernetes.io/managed-by=gocdnext)" >&2
                exit 2
            fi
            prune_args+=("--prune" "-l" "${PLUGIN_PRUNE_LABEL}")
        fi

        # shellcheck disable=SC2086
        printf '%s\n' "${manifests}" \
            | kubectl apply "${ns_args[@]}" "${prune_args[@]}" -f - ${PLUGIN_EXTRA_ARGS:-}
        ;;
esac
