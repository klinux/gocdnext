#!/bin/bash
# gocdnext/trivy — CVE + secret scanner. See Dockerfile for the
# full input contract.

set -euo pipefail

SCAN_TYPE="${PLUGIN_SCAN_TYPE:-fs}"
SEVERITY="${PLUGIN_SEVERITY:-HIGH,CRITICAL}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"
IGNORE_UNFIXED="${PLUGIN_IGNORE_UNFIXED:-true}"
FORMAT="${PLUGIN_FORMAT:-table}"

# Default target per scan type: workspace root for fs/config,
# required-otherwise for image/repo (a scan with no target is a
# mistake worth flagging at runtime, not a silent no-op).
TARGET="${PLUGIN_TARGET:-}"
case "${SCAN_TYPE}" in
  fs|config)
    TARGET="${TARGET:-/workspace}"
    ;;
  image|repo)
    if [ -z "${TARGET}" ]; then
      echo "gocdnext/trivy: PLUGIN_TARGET is required when scan_type=${SCAN_TYPE}" >&2
      exit 2
    fi
    ;;
  *)
    echo "gocdnext/trivy: unknown scan_type ${SCAN_TYPE} (accepted: image, fs, repo, config)" >&2
    exit 2
    ;;
esac

args=(
  "${SCAN_TYPE}"
  "--severity" "${SEVERITY}"
  "--exit-code" "${EXIT_CODE}"
  "--format" "${FORMAT}"
)

# --ignore-unfixed only makes sense on CVE scans; it has no
# effect on config/secret rules but trivy doesn't reject it
# either — always forwarding keeps the flag list simple.
if [ "${IGNORE_UNFIXED}" = "true" ]; then
  args+=("--ignore-unfixed")
fi

if [ -n "${PLUGIN_OUTPUT:-}" ]; then
  args+=("--output" "/workspace/${PLUGIN_OUTPUT}")
fi

args+=("${TARGET}")

echo "==> trivy ${args[*]}"
exec trivy "${args[@]}"
