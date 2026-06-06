#!/bin/bash
# gocdnext/cosign — sign / verify / attest container images via
# cosign (Sigstore). See Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_IMAGE:-}" ]; then
    echo "gocdnext/cosign: PLUGIN_IMAGE is required" >&2
    echo "  example: image: ghcr.io/org/app@sha256:..." >&2
    exit 2
fi

ACTION="${PLUGIN_ACTION:-sign}"
KEY="${PLUGIN_KEY:-}"

# key-content lets operators pipe the key BYTES directly through
# `secrets:` + `with: { key-content: ${{ COSIGN_PRIVATE_KEY }} }`
# instead of writing the key to an artifact (which would persist
# in the artifact backend) or pre-staging via a script step. The
# entrypoint writes the content to a private tempfile that the
# trap wipes on exit, regardless of cosign's exit status.
#
# `key:` (path) and `key-content:` are mutually exclusive — the
# entrypoint aborts if both are set so the operator picks one
# consistently rather than relying on which-wins precedence.
if [ -n "${PLUGIN_KEY_CONTENT:-}" ]; then
    if [ -n "${KEY}" ]; then
        echo "gocdnext/cosign: key: and key-content: are mutually exclusive — pick one" >&2
        exit 2
    fi
    KEY_TEMPFILE=$(mktemp)
    chmod 600 "${KEY_TEMPFILE}"
    trap 'rm -f "${KEY_TEMPFILE}"' EXIT INT TERM
    printf '%s' "${PLUGIN_KEY_CONTENT}" > "${KEY_TEMPFILE}"
    KEY="${KEY_TEMPFILE}"
fi

# Guard against `key: ${{ COSIGN_PRIVATE_KEY }}` — the legacy
# pattern that some recipes (and old runbooks) still document.
# `key:` is a FILE PATH; passing the PEM content directly would
# both leak via the "==> cosign --key <content>" echo below AND
# place the secret on cosign's own argv. Detect PEM-like input or
# multi-line values and bail loud with a remediation hint.
if [ -n "${KEY}" ]; then
    case "${KEY}" in
        *"-----BEGIN"*|*$'\n'*)
            echo "gocdnext/cosign: key: must be a FILE PATH, not the key bytes" >&2
            echo "  for inline PEM content from a secret, use key-content: instead:" >&2
            echo "    key-content: \${{ COSIGN_PRIVATE_KEY }}" >&2
            echo "  the plugin writes it to a 0600 tempfile internally" >&2
            echo "  and a trap wipes it on exit — no argv leak, no artifact" >&2
            echo "  persistence." >&2
            exit 2
            ;;
    esac
fi

if [ -n "${PLUGIN_USERNAME:-}" ]; then
    registry="${PLUGIN_REGISTRY:-}"
    if [ -z "${registry}" ]; then
        if [[ "${PLUGIN_IMAGE}" == */* ]]; then
            registry="${PLUGIN_IMAGE%%/*}"
        else
            registry="docker.io"
        fi
    fi
    echo "==> cosign login ${registry} as ${PLUGIN_USERNAME}"
    echo "${PLUGIN_PASSWORD:-}" | cosign login "${registry}" \
        --username "${PLUGIN_USERNAME}" --password-stdin
fi

# Unset COSIGN_EXPERIMENTAL default: as of cosign 2.x, keyless is
# GA and does not need the flag. Be explicit so an old operator
# runbook that sets it doesn't confuse the output.
unset COSIGN_EXPERIMENTAL || true

# cosign is invoked as a CHILD process (not via `exec`) so the
# bash EXIT trap actually fires — `exec` would replace bash with
# cosign and the trap (registered to clean up the key-content
# tempfile, when used) would never run. Performance cost is one
# extra fork; security cost of `exec` would be a leaked key file.
#
# echo_cmd_redacted prints the cosign invocation but replaces the
# value of `--key` with [REDACTED] in the displayed string.
# Defence in depth: when the operator's PEM is a file path
# (which the guard above enforced), the path itself isn't
# sensitive — but redacting unconditionally protects any future
# `--key <content>` case the guard might miss, and makes the
# audit log uniform.
echo_cmd_redacted() {
    local args=("$@") display=()
    local skip=false
    for arg in "${args[@]}"; do
        if [ "${skip}" = "true" ]; then
            display+=("[REDACTED]")
            skip=false
            continue
        fi
        case "${arg}" in
            --key|--key-password|--password|-p)
                display+=("${arg}")
                skip=true
                ;;
            *)
                display+=("${arg}")
                ;;
        esac
    done
    echo "==> cosign ${display[*]}"
}
case "${ACTION}" in
    sign)
        cmd=(sign --yes)
        if [ -n "${KEY}" ]; then
            cmd+=(--key "${KEY}")
            if [ -n "${PLUGIN_KEY_PASSWORD:-}" ]; then
                export COSIGN_PASSWORD="${PLUGIN_KEY_PASSWORD}"
            fi
        fi
        cmd+=("${PLUGIN_IMAGE}")
        echo_cmd_redacted "${cmd[@]}"
        cosign "${cmd[@]}"
        ;;
    verify)
        cmd=(verify)
        if [ -n "${KEY}" ]; then
            cmd+=(--key "${KEY}")
        else
            if [ -z "${PLUGIN_CERT_IDENTITY:-}" ] || [ -z "${PLUGIN_CERT_OIDC_ISSUER:-}" ]; then
                echo "gocdnext/cosign: keyless verify needs cert-identity + cert-oidc-issuer" >&2
                exit 2
            fi
            cmd+=(--certificate-identity "${PLUGIN_CERT_IDENTITY}"
                  --certificate-oidc-issuer "${PLUGIN_CERT_OIDC_ISSUER}")
        fi
        cmd+=("${PLUGIN_IMAGE}")
        echo_cmd_redacted "${cmd[@]}"
        cosign "${cmd[@]}"
        ;;
    attest)
        if [ -z "${PLUGIN_PREDICATE:-}" ]; then
            echo "gocdnext/cosign: action=attest needs PLUGIN_PREDICATE" >&2
            exit 2
        fi
        cmd=(attest --yes --predicate "${PLUGIN_PREDICATE}")
        if [ -n "${PLUGIN_PREDICATE_TYPE:-}" ]; then
            cmd+=(--type "${PLUGIN_PREDICATE_TYPE}")
        fi
        if [ -n "${KEY}" ]; then
            cmd+=(--key "${KEY}")
            if [ -n "${PLUGIN_KEY_PASSWORD:-}" ]; then
                export COSIGN_PASSWORD="${PLUGIN_KEY_PASSWORD}"
            fi
        fi
        cmd+=("${PLUGIN_IMAGE}")
        echo_cmd_redacted "${cmd[@]}"
        cosign "${cmd[@]}"
        ;;
    *)
        echo "gocdnext/cosign: unknown action '${ACTION}' (accepted: sign, verify, attest)" >&2
        exit 2
        ;;
esac
