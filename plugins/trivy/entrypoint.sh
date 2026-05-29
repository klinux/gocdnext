#!/bin/bash
# gocdnext/trivy — CVE + secret scanner. See Dockerfile for the
# full input contract.

set -euo pipefail

SCAN_TYPE="${PLUGIN_SCAN_TYPE:-fs}"
SEVERITY="${PLUGIN_SEVERITY:-HIGH,CRITICAL}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"
IGNORE_UNFIXED="${PLUGIN_IGNORE_UNFIXED:-true}"
FORMAT="${PLUGIN_FORMAT:-table}"

# Trivy ships its CVE DB out-of-band — ~50MB pulled from
# ghcr.io/aquasecurity/trivy-db on every fresh scan. Pinning
# TRIVY_CACHE_DIR to a PWD-relative path makes the cache survive
# across runs as long as the platform's `cache:` block persists
# `.cache/trivy`. Trivy still checks the DB age on every run
# (default policy: refresh if older than 6h) — caching just turns
# the COLD path (download) into a HEAD-only freshness check on
# warm runs. Override via `variables: TRIVY_CACHE_DIR: ...` for
# operators who want to point at a node-level shared cache.
#
# Recommended cache block:
#   cache:
#     - key: trivy-db
#       paths:
#         - .cache/trivy
export TRIVY_CACHE_DIR="${TRIVY_CACHE_DIR:-.cache/trivy}"
mkdir -p "${TRIVY_CACHE_DIR}"

# Default target per scan type: PWD for fs/config (resolves to
# the checkout dir via the container's WorkingDir, matching what
# every other plugin does), required-otherwise for image/repo
# (a scan with no target is a mistake worth flagging at runtime,
# not a silent no-op).
TARGET="${PLUGIN_TARGET:-}"
case "${SCAN_TYPE}" in
  fs|config)
    TARGET="${TARGET:-.}"
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

# Trivy's default behaviour with a persistent TRIVY_CACHE_DIR
# already does what we want: HEAD upstream to check DB freshness
# (<200ms), only download if stale (default 24h policy). No
# extra flag needed. Power users can force `skip_db_update: true`
# in YAML to skip the HEAD entirely — useful for offline / fully
# air-gapped runners, where the freshness check itself would
# fail. Default is OFF: be fast AND correct.
if [ "${PLUGIN_SKIP_DB_UPDATE:-false}" = "true" ]; then
  args+=("--skip-db-update")
fi

# --ignore-unfixed only makes sense on CVE scans; it has no
# effect on config/secret rules but trivy doesn't reject it
# either — always forwarding keeps the flag list simple.
if [ "${IGNORE_UNFIXED}" = "true" ]; then
  args+=("--ignore-unfixed")
fi

if [ -n "${PLUGIN_OUTPUT:-}" ]; then
  args+=("--output" "${PLUGIN_OUTPUT}")
fi

args+=("${TARGET}")

echo "==> trivy ${args[*]}"
exec trivy "${args[@]}"
