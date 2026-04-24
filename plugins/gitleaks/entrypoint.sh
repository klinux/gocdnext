#!/bin/bash
# gocdnext/gitleaks — thin wrapper around gitleaks. See
# Dockerfile for the full contract.

set -euo pipefail

PATH_TO_SCAN="${PLUGIN_PATH:-.}"
FORMAT="${PLUGIN_FORMAT:-json}"
MODE="${PLUGIN_SCAN_MODE:-dir}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"

# dir vs git: `detect` scans files under --source; `git` walks
# commit history. 90% of CI users want the former — "did I just
# commit a secret now?" not "was one ever committed in this
# repo's lifetime?". Keep the gitleaks exit code intact so the
# caller can tune strictness via PLUGIN_EXIT_CODE (0 for
# advisory).
case "${MODE}" in
  dir)
    cmd=(detect --source "/workspace/${PATH_TO_SCAN}" --no-git)
    ;;
  git)
    cmd=(detect --source "/workspace/${PATH_TO_SCAN}")
    ;;
  *)
    echo "gocdnext/gitleaks: unknown scan_mode ${MODE} (accepted: dir, git)" >&2
    exit 2
    ;;
esac

cmd+=("--exit-code" "${EXIT_CODE}" "--report-format" "${FORMAT}")

if [ -n "${PLUGIN_CONFIG:-}" ]; then
  cmd+=("--config" "/workspace/${PLUGIN_CONFIG}")
fi

if [ -n "${PLUGIN_REPORT:-}" ]; then
  cmd+=("--report-path" "/workspace/${PLUGIN_REPORT}")
fi

# Git 2.35 dubious-ownership workaround, same as every other
# plugin that touches git metadata.
git config --global --add safe.directory '*' 2>/dev/null || true

echo "==> gitleaks ${cmd[*]}"
exec gitleaks "${cmd[@]}"
