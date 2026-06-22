#!/bin/bash
# gocdnext/kustomize — render a kustomization tree and apply / build /
# diff / validate it against a cluster. Optionally set image tags,
# substitute ${VAR} placeholders from the job env, and wait for the
# rollout to become healthy. See plugin.yaml + Dockerfile for the full
# input contract.
#
# Design: this is the ONE structured deploy plugin (path + a handful of
# declarative flags). Ad-hoc kubectl/helm operations belong in the
# gocdnext/kubectl + gocdnext/helm passthrough plugins; rollback +
# version tracking belong to the `deploy:` marker; kubeconfig to the
# cluster registry. This plugin only renders → applies → waits.

set -euo pipefail

err() { echo "gocdnext/kustomize: $*" >&2; }

if [ -z "${PLUGIN_PATH:-}" ]; then
    err "PLUGIN_PATH is required (e.g. path: deploy/overlays/prod)"
    exit 2
fi
if [ ! -d "${PLUGIN_PATH}" ]; then
    err "path '${PLUGIN_PATH}' is not a directory under /workspace"
    exit 2
fi

action="${PLUGIN_ACTION:-apply}"
case "${action}" in
    apply | build | diff | validate) ;;
    *)
        err "unknown action '${action}' (use apply|build|diff|validate)"
        exit 2
        ;;
esac

# ── Kubeconfig discovery — mirrors the kubectl + helm plugins verbatim
#    so operators juggle one convention. path / inline YAML / base64
#    YAML, auto-detected; empty defers to the agent's in-cluster SA.
#    (The cluster registry's `cluster:` key feeds PLUGIN_KUBECONFIG.) ──
if [ -n "${PLUGIN_KUBECONFIG:-}" ]; then
    dest=/tmp/gocdnext-kubeconfig
    if [ -f "${PLUGIN_KUBECONFIG}" ]; then
        cp "${PLUGIN_KUBECONFIG}" "${dest}"
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

# ── Set image tags before rendering (kustomize edit set image). ──
# PLUGIN_IMAGES: newline- or comma-separated entries, each a kustomize
# image override — "name=registry/img:tag" or "name=registry/img@sha256:..".
# Deploying a freshly built image is the dominant CD case; this keeps the
# tag out of the committed kustomization (the overlay pins a placeholder,
# the pipeline sets the real tag from CI_COMMIT_SHORT_SHA / an output).
if [ -n "${PLUGIN_IMAGES:-}" ]; then
    echo "--- setting images ---"
    while IFS= read -r img; do
        img="$(printf '%s' "${img}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')" # trim
        [ -z "${img}" ] && continue
        echo "  ${img}"
        (cd "${PLUGIN_PATH}" && kustomize edit set image "${img}")
    done <<<"$(printf '%s' "${PLUGIN_IMAGES}" | tr ',' '\n')"
fi

# ── Substitute ${VAR} placeholders from the job env, BEFORE render. ──
# PLUGIN_ENVSUBST: "true" substitutes every variable CURRENTLY SET in the
# env; a comma/space list ("DB_PASSWORD,API_TOKEN") restricts to those
# names — the recommended form for injecting project secrets (the value is
# in the masked job env; the source holds ${NAME}, never committed).
#
# Both modes substitute ONLY set variables, so an unset ${...} survives
# untouched. This is why we always pass an explicit SHELL-FORMAT to
# envsubst, even for "true": a bare `envsubst` would replace an unset
# ${FOO} with "" and silently blank out placeholders in a base.
#
# Done IN PLACE on *.yaml/*.yml under PLUGIN_PATH *before* `kustomize
# build` so the source quoting is preserved — `value: "${SECRET}"` stays
# quoted, which keeps a secret with YAML-special chars valid. (Doing it
# on the rendered output would be unsafe: kustomize strips quotes, so a
# special-char value could break parsing.) The workspace is ephemeral, so
# the in-place edit is safe. NOTE: only files under PLUGIN_PATH are
# processed — placeholders in a base outside the path are not substituted.
if [ -n "${PLUGIN_ENVSUBST:-}" ] && [ "${PLUGIN_ENVSUBST}" != "false" ]; then
    if [ "${PLUGIN_ENVSUBST}" = "true" ]; then
        # Every variable set in the env (names only — unset ${...} survive).
        shellfmt="$(compgen -e | sed 's/^/$/' | paste -sd' ' -)"
    else
        # Explicit allowlist; validate each is an env identifier so a typo
        # fails loud instead of silently substituting nothing.
        shellfmt=""
        while IFS= read -r name; do
            [ -z "${name}" ] && continue
            if ! printf '%s' "${name}" | grep -qE '^[A-Za-z_][A-Za-z0-9_]*$'; then
                err "envsubst: '${name}' is not a valid variable name"
                exit 2
            fi
            # Fail loud if a listed var is absent: substituting it would
            # blank ${name} to "" — a silently empty secret is worse than a
            # hard failure. (envsubst '$MISSING' → empty.)
            if ! [[ -v "${name}" ]]; then
                err "envsubst: '${name}' is listed but not set in the env (register it as a secret/var)"
                exit 2
            fi
            shellfmt="${shellfmt:+${shellfmt} }\$${name}"
        done < <(printf '%s\n' "${PLUGIN_ENVSUBST}" | tr ', ' '\n')
    fi
    while IFS= read -r -d '' f; do
        envsubst "${shellfmt}" <"${f}" >"${f}.envsubst"
        mv "${f}.envsubst" "${f}"
    done < <(find "${PLUGIN_PATH}" -type f \( -name '*.yaml' -o -name '*.yml' \) -print0)
fi

# ── Render. --enable-helm inflates `helmCharts:` entries. ──
build_args=()
if [ "${PLUGIN_ENABLE_HELM:-false}" = "true" ]; then
    build_args+=("--enable-helm")
fi
manifests="$(kustomize build "${build_args[@]}" "${PLUGIN_PATH}")"

emit() { printf '%s\n' "${manifests}"; }

case "${action}" in
    build)
        emit
        ;;

    validate)
        echo "--- validating (server dry-run) ---"
        emit | kubectl apply "${ns_args[@]}" --dry-run=server -f -
        ;;

    diff)
        emit | kubectl diff "${ns_args[@]}" -f - || diff_rc=$?
        # kubectl diff exits 1 when there are differences — not a job
        # failure; only ≥2 (a real kubectl error) fails the job.
        if [ "${diff_rc:-0}" -ge 2 ]; then
            exit "${diff_rc}"
        fi
        ;;

    apply)
        # Echo the rendered manifest for traceability — but NOT when
        # envsubst is active. The runner's log masker handles single-line
        # values ≥4 chars, yet a multi-line PEM/cert or a short value can
        # slip through, so a resolved secret could land in the log. kubectl
        # still receives the real manifest over the pipe below.
        if [ -n "${PLUGIN_ENVSUBST:-}" ] && [ "${PLUGIN_ENVSUBST}" != "false" ]; then
            echo "--- rendered manifests omitted (envsubst enabled — avoids logging injected secrets) ---"
        else
            echo "--- rendered manifests ---"
            emit
        fi
        echo "--- applying ---"

        if [ "${PLUGIN_ENSURE_NAMESPACE:-false}" = "true" ]; then
            if [ -z "${PLUGIN_NAMESPACE:-}" ]; then
                err "ensure_namespace=true requires namespace"
                exit 2
            fi
            kubectl get namespace "${PLUGIN_NAMESPACE}" >/dev/null 2>&1 \
                || kubectl create namespace "${PLUGIN_NAMESPACE}"
        fi

        apply_args=("${ns_args[@]}")
        if [ "${PLUGIN_SERVER_SIDE:-false}" = "true" ]; then
            apply_args+=("--server-side")
        fi
        if [ "${PLUGIN_PRUNE:-false}" = "true" ]; then
            if [ -z "${PLUGIN_PRUNE_LABEL:-}" ]; then
                err "prune=true requires prune_label (e.g. app.kubernetes.io/managed-by=gocdnext)"
                exit 2
            fi
            apply_args+=("--prune" "-l" "${PLUGIN_PRUNE_LABEL}")
        fi

        # shellcheck disable=SC2086
        emit | kubectl apply "${apply_args[@]}" -f - ${PLUGIN_EXTRA_ARGS:-}

        # ── Wait for the rollout so deploy-success == rolled-out, not just
        #    "submitted". Workloads + their namespace are parsed from the
        #    already-rendered manifests (offline — no server roundtrip).
        #    kustomize normalises to block YAML with metadata fields at
        #    2-space indent, so the awk scan is reliable on its own output.
        #    Each workload's rollout is queried in the SAME namespace it was
        #    applied to: the manifest's metadata.namespace wins (matching
        #    `kubectl apply`), else PLUGIN_NAMESPACE, else the context default. ──
        if [ "${PLUGIN_WAIT:-false}" = "true" ]; then
            timeout="${PLUGIN_WAIT_TIMEOUT:-120s}"
            echo "--- waiting for rollout (timeout ${timeout}) ---"
            while IFS='|' read -r res mns; do
                [ -z "${res}" ] && continue
                rollout_ns=()
                if [ -n "${mns}" ]; then
                    rollout_ns=("--namespace" "${mns}")
                elif [ -n "${PLUGIN_NAMESPACE:-}" ]; then
                    rollout_ns=("--namespace" "${PLUGIN_NAMESPACE}")
                fi
                if ! kubectl rollout status "${res}" "${rollout_ns[@]}" --timeout="${timeout}"; then
                    err "rollout failed for ${res} — diagnostics below"
                    kubectl get events "${rollout_ns[@]}" --sort-by=.lastTimestamp 2>/dev/null | tail -n 30 || true
                    kubectl describe "${res}" "${rollout_ns[@]}" 2>/dev/null || true
                    exit 1
                fi
            done < <(emit | awk '
                function flush() { if (kind ~ /^(Deployment|StatefulSet|DaemonSet)$/ && name != "") print kind "/" name "|" ns }
                /^---/         { flush(); kind=""; name=""; ns=""; next }
                /^kind:/       { kind=$2 }
                /^  name:/      && name=="" { name=$2 }
                /^  namespace:/ && ns==""   { ns=$2 }
                END { flush() }
            ')
            echo "rollout complete"
        fi
        ;;
esac
