#!/bin/bash
# gocdnext/trivy — CVE + secret scanner. See Dockerfile for the
# full input contract.

set -euo pipefail

SCAN_TYPE="${PLUGIN_SCAN_TYPE:-fs}"
SEVERITY="${PLUGIN_SEVERITY:-HIGH,CRITICAL}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"
IGNORE_UNFIXED="${PLUGIN_IGNORE_UNFIXED:-true}"
FORMAT="${PLUGIN_FORMAT:-table}"

# Registry auth for `scan-type: image` against a private registry.
# Trivy reads TRIVY_USERNAME / TRIVY_PASSWORD directly from env;
# we promote PLUGIN_USERNAME / PLUGIN_PASSWORD to those names so
# operators get a consistent "pipe via secrets:" UX with every
# other plugin in the catalog. Values come from the agent's
# env-propagation path (post-v0.7.x: NAME-only on argv, value
# inherited from cmd.Env), so they don't appear in `ps auxww`.
if [ -n "${PLUGIN_USERNAME:-}" ]; then
    export TRIVY_USERNAME="${PLUGIN_USERNAME}"
fi
if [ -n "${PLUGIN_PASSWORD:-}" ]; then
    export TRIVY_PASSWORD="${PLUGIN_PASSWORD}"
fi

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

# Misconfiguration checks (scan_type=config) ship as a rego bundle trivy fetches
# from the registry at runtime. That bundle moves forward independently of the
# pinned trivy binary, so a newer bundle can reference a schema the binary
# doesn't know and fail to compile — the `[rego] undefined ref … requestedamis`
# / "Failed to find embedded check, skipping" noise, with silent coverage loss.
# Use the checks EMBEDDED in the binary instead: --skip-check-update makes the
# scan deterministic (same plugin version => same checks), offline, and
# skew-free. Move the check set by bumping the trivy binary in the Dockerfile,
# not by whatever the registry served today.
if [ "${SCAN_TYPE}" = "config" ]; then
  args+=("--skip-check-update")
fi

# --ignore-unfixed applies only to CVE scans (image/fs/repo). `trivy config`
# is a misconfig scan with no fixed/unfixed axis and REJECTS the flag with
# `unknown flag: --ignore-unfixed` (FATAL), so gate it out for config. The
# default is on (IGNORE_UNFIXED defaults to true), so a config scan that never
# sets it would otherwise inherit the flag and die.
if [ "${IGNORE_UNFIXED}" = "true" ] && [ "${SCAN_TYPE}" != "config" ]; then
  args+=("--ignore-unfixed")
fi

# --skip-dirs: comma-separated dirs/globs to exclude from an fs/repo
# scan (e.g. "node_modules,vendor" — build-time deps that never reach
# the runtime image, so their CVEs are noise on a source scan). trivy
# treats the value as a comma-separated list, so it rides as one arg.
if [ -n "${PLUGIN_SKIP_DIRS:-}" ]; then
  args+=("--skip-dirs" "${PLUGIN_SKIP_DIRS}")
fi

if [ -n "${PLUGIN_OUTPUT:-}" ]; then
  args+=("--output" "${PLUGIN_OUTPUT}")
fi

args+=("${TARGET}")

echo "==> trivy ${args[*]}"

# Run the scan. Capture the exit code instead of exec'ing so we can add a
# human-readable summary on failure (below) before propagating it.
set +e
trivy "${args[@]}"
rc=$?
set -e

# When the report was written to a FILE in a machine format (sarif/json/
# cyclonedx), nothing about the findings reached the job log — a failing gate
# then shows only the exit code plus any unrelated check-loading noise, so
# operators mistake the noise for the cause. On a non-zero exit under those
# conditions, re-run the SAME scan as a human table (to stdout, --exit-code 0)
# so the findings that ACTUALLY failed the gate are visible in the log. Best-
# effort: it never changes the real exit code.
if [ "${rc}" -ne 0 ] && [ -n "${PLUGIN_OUTPUT:-}" ] && [ "${FORMAT}" != "table" ]; then
  echo "── trivy: findings that failed the gate (severity ${SEVERITY}) ──"
  summary=("${SCAN_TYPE}" "--severity" "${SEVERITY}" "--format" "table" "--exit-code" "0")
  # Mirror the primary run's update-skipping flags so the summary works on the
  # same (possibly offline/air-gapped) runner — otherwise it would try to touch
  # the network for DB/check freshness and fail exactly when we need the table.
  if [ "${PLUGIN_SKIP_DB_UPDATE:-false}" = "true" ]; then
    summary+=("--skip-db-update")
  fi
  if [ "${SCAN_TYPE}" = "config" ]; then
    summary+=("--skip-check-update")
  fi
  if [ "${IGNORE_UNFIXED}" = "true" ] && [ "${SCAN_TYPE}" != "config" ]; then
    summary+=("--ignore-unfixed")
  fi
  if [ -n "${PLUGIN_SKIP_DIRS:-}" ]; then
    summary+=("--skip-dirs" "${PLUGIN_SKIP_DIRS}")
  fi
  summary+=("${TARGET}")
  trivy "${summary[@]}" || true
fi

exit "${rc}"
