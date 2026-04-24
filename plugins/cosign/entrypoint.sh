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

case "${ACTION}" in
    sign)
        cmd=(sign --yes)
        if [ -n "${KEY}" ]; then
            cmd+=(--key "/workspace/${KEY}")
            if [ -n "${PLUGIN_KEY_PASSWORD:-}" ]; then
                export COSIGN_PASSWORD="${PLUGIN_KEY_PASSWORD}"
            fi
        fi
        cmd+=("${PLUGIN_IMAGE}")
        echo "==> cosign ${cmd[*]}"
        exec cosign "${cmd[@]}"
        ;;
    verify)
        cmd=(verify)
        if [ -n "${KEY}" ]; then
            cmd+=(--key "/workspace/${KEY}")
        else
            if [ -z "${PLUGIN_CERT_IDENTITY:-}" ] || [ -z "${PLUGIN_CERT_OIDC_ISSUER:-}" ]; then
                echo "gocdnext/cosign: keyless verify needs cert-identity + cert-oidc-issuer" >&2
                exit 2
            fi
            cmd+=(--certificate-identity "${PLUGIN_CERT_IDENTITY}"
                  --certificate-oidc-issuer "${PLUGIN_CERT_OIDC_ISSUER}")
        fi
        cmd+=("${PLUGIN_IMAGE}")
        echo "==> cosign ${cmd[*]}"
        exec cosign "${cmd[@]}"
        ;;
    attest)
        if [ -z "${PLUGIN_PREDICATE:-}" ]; then
            echo "gocdnext/cosign: action=attest needs PLUGIN_PREDICATE" >&2
            exit 2
        fi
        cmd=(attest --yes --predicate "/workspace/${PLUGIN_PREDICATE}")
        if [ -n "${PLUGIN_PREDICATE_TYPE:-}" ]; then
            cmd+=(--type "${PLUGIN_PREDICATE_TYPE}")
        fi
        if [ -n "${KEY}" ]; then
            cmd+=(--key "/workspace/${KEY}")
            if [ -n "${PLUGIN_KEY_PASSWORD:-}" ]; then
                export COSIGN_PASSWORD="${PLUGIN_KEY_PASSWORD}"
            fi
        fi
        cmd+=("${PLUGIN_IMAGE}")
        echo "==> cosign ${cmd[*]}"
        exec cosign "${cmd[@]}"
        ;;
    *)
        echo "gocdnext/cosign: unknown action '${ACTION}' (accepted: sign, verify, attest)" >&2
        exit 2
        ;;
esac
