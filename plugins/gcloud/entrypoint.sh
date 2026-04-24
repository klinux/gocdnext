#!/bin/bash
# gocdnext/gcloud — thin wrapper around `gcloud`. See Dockerfile
# for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/gcloud: PLUGIN_COMMAND is required" >&2
    echo "  example: command: run deploy svc --image gcr.io/p/app:1" >&2
    exit 2
fi

WORKING_DIR="${PLUGIN_WORKING_DIR:-.}"
cd "/workspace/${WORKING_DIR}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

# Auth path 1: raw JSON key from secrets → tempfile → activate.
# We do NOT set GOOGLE_APPLICATION_CREDENTIALS to the same file
# (gcloud picks up the activated account by itself, and
# clobbering GAC breaks Workload Identity flows where the user
# already set it).
if [ -n "${PLUGIN_CREDENTIALS_JSON:-}" ]; then
    keyfile="${tmpdir}/gcp-sa.json"
    printf '%s' "${PLUGIN_CREDENTIALS_JSON}" >"${keyfile}"
    chmod 600 "${keyfile}"
    echo "==> activating service account from secrets"
    gcloud auth activate-service-account --key-file="${keyfile}" >/dev/null
fi

project_args=()
if [ -n "${PLUGIN_PROJECT:-}" ]; then
    project_args+=("--project" "${PLUGIN_PROJECT}")
fi

# Intentional word-splitting: "run deploy x --image y" → 4 args.
# shellcheck disable=SC2086
echo "==> gcloud ${PLUGIN_COMMAND} ${project_args[*]:-}"
# shellcheck disable=SC2086
exec gcloud ${PLUGIN_COMMAND} "${project_args[@]}"
