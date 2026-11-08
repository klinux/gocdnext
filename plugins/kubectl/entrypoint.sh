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
    # The config holds tokens/certs, so it MUST be 0600 from the first byte.
    # mktemp gives a FRESH, unique, 0600 file — a fixed path could preexist
    # permissive (and redirection/cp into an existing inode inherit its mode, so
    # neither umask nor chmod-after reliably closes the window). The template is
    # ANCHORED to /tmp: a bare `mktemp` honours $TMPDIR, which a job could point at
    # /workspace (a persisted/artifacted volume), leaking the tokens/certs out of
    # the container. Then write with REDIRECTION (not `cp`, which copies the SOURCE
    # file's mode) so the 0600 mktemp set is preserved. entrypoint_test.sh asserts
    # the result is 0600 at a fresh /tmp path.
    dest="$(mktemp /tmp/gocdnext-kubeconfig.XXXXXX)"
    if [ -f "${PLUGIN_KUBECONFIG}" ]; then
        cat "${PLUGIN_KUBECONFIG}" >"${dest}"
    elif echo "${PLUGIN_KUBECONFIG}" | base64 -d >"${dest}" 2>/dev/null \
         && kubectl config view --kubeconfig "${dest}" >/dev/null 2>&1; then
        : # the base64 DECODED to a kubeconfig kubectl itself accepts (validated
          # over the whole file, not a byte-window heuristic).
    else
        # Treat as literal YAML — covers the inline case.
        printf '%s' "${PLUGIN_KUBECONFIG}" >"${dest}"
    fi
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

# `set -f` disables pathname (glob) expansion: an unquoted ${PLUGIN_COMMAND}
# would otherwise expand `*`/`?`/`[...]` against the workspace and silently
# rewrite args. Word-splitting (the intended behaviour) still applies.
set -f

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
