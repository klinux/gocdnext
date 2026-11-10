#!/bin/bash
# gocdnext/lighthouse-ci — collect + (optionally) assert + upload
# Lighthouse results. See Dockerfile for the full contract.

set -euo pipefail


# LHCI CLI is installed globally on the base image as `lhci`.
# Collect stage: either point at URLs directly or honour an
# explicit / auto-discovered lighthouserc file.

collect_args=()
if [ -n "${PLUGIN_CONFIG:-}" ]; then
    cfg="${PLUGIN_CONFIG#/}"
    if [ ! -f "${cfg}" ]; then
        echo "gocdnext/lighthouse-ci: config ${PLUGIN_CONFIG} not found" >&2
        exit 2
    fi
    collect_args+=(--config="${cfg}")
elif [ -n "${PLUGIN_URLS:-}" ]; then
    # Accept newline OR comma separators. Users pasting from a
    # YAML multi-line string get newlines; inline `urls: "a,b"`
    # gets commas.
    urls_raw="${PLUGIN_URLS//,/$'\n'}"
    while IFS= read -r u; do
        u_trimmed="${u## }"
        u_trimmed="${u_trimmed%% }"
        [ -z "${u_trimmed}" ] && continue
        collect_args+=(--url="${u_trimmed}")
    done <<<"${urls_raw}"
else
    # LHCI auto-discovers ./lighthouserc.{json,yml,js} — letting
    # the CLI handle that case keeps the contract small.
    echo "==> no urls / config input; relying on LHCI auto-discovery"
fi

if [ -n "${PLUGIN_NUMBER_OF_RUNS:-}" ]; then
    collect_args+=(--numberOfRuns="${PLUGIN_NUMBER_OF_RUNS}")
fi

echo "==> lhci collect ${collect_args[*]}"
lhci collect "${collect_args[@]}"

# Assert stage — reads the config's `assert` section. When
# assertions are off, skip this step entirely; the collect output
# is still available for upload.
if [ "${PLUGIN_ASSERTIONS:-on}" = "on" ]; then
    echo "==> lhci assert"
    lhci assert
fi

# Upload stage.
target="${PLUGIN_UPLOAD_TARGET:-temporary-public-storage}"
upload_args=(--target="${target}")
case "${target}" in
    temporary-public-storage|filesystem)
        ;;
    lhci)
        if [ -z "${PLUGIN_UPLOAD_SERVER_BASE_URL:-}" ]; then
            echo "gocdnext/lighthouse-ci: upload-target=lhci needs upload-server-base-url" >&2
            exit 2
        fi
        upload_args+=(--serverBaseUrl="${PLUGIN_UPLOAD_SERVER_BASE_URL}")
        # The build token is a secret. Hand it to LHCI via the LHCI_TOKEN
        # env var (the CLI maps LHCI_* env through yargs' .env('LHCI') to the
        # upload `token` option) instead of a --token flag, so it never lands
        # on the child's argv (readable via ps / /proc/<pid>/cmdline) and never
        # reaches the `==> lhci upload ...` log line below. The server base URL
        # stays a flag — it is not a secret.
        if [ -n "${PLUGIN_UPLOAD_TOKEN:-}" ]; then
            export LHCI_TOKEN="${PLUGIN_UPLOAD_TOKEN}"
        fi
        ;;
    *)
        echo "gocdnext/lighthouse-ci: unknown upload-target '${target}'" >&2
        exit 2
        ;;
esac

echo "==> lhci upload ${upload_args[*]}"
exec lhci upload "${upload_args[@]}"
