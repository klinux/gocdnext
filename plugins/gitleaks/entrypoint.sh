#!/bin/bash
# gocdnext/gitleaks — thin wrapper around gitleaks. See
# Dockerfile for the full contract.

set -euo pipefail

PATH_TO_SCAN="${PLUGIN_PATH:-.}"
FORMAT="${PLUGIN_FORMAT:-json}"
MODE="${PLUGIN_SCAN_MODE:-dir}"
EXIT_CODE="${PLUGIN_EXIT_CODE:-1}"
VERBOSE="${PLUGIN_VERBOSE:-true}"
REDACT="${PLUGIN_REDACT:-75}"

# dir vs git: `detect` scans files under --source; `git` walks
# commit history. 90% of CI users want the former — "did I just
# commit a secret now?" not "was one ever committed in this
# repo's lifetime?". Keep the gitleaks exit code intact so the
# caller can tune strictness via PLUGIN_EXIT_CODE (0 for
# advisory).
case "${MODE}" in
  dir)
    cmd=(detect --source "${PATH_TO_SCAN}" --no-git)
    ;;
  git)
    cmd=(detect --source "${PATH_TO_SCAN}")
    ;;
  *)
    echo "gocdnext/gitleaks: unknown scan_mode ${MODE} (accepted: dir, git)" >&2
    exit 2
    ;;
esac

cmd+=("--exit-code" "${EXIT_CODE}" "--report-format" "${FORMAT}")

# --verbose prints each finding's file:line + rule + redacted
# secret to stderr as gitleaks discovers them. Without this the
# operator sees only "leaks found: 13" and has to dig through a
# separately-shipped JSON report to find out WHICH files —
# essentially useless as immediate CI feedback. Default on; set
# `verbose: false` to silence (still hits exit-code 1 on findings,
# the JSON report still gets written if --report-path is set).
if [ "${VERBOSE}" != "false" ]; then
  cmd+=("--verbose")
fi

# --redact masks N% of the secret in the verbose output so the
# log shows ENOUGH of the value to confirm "yes that's my AWS
# key" without leaving the key in plaintext in the build log
# stream. 75% redact leaves prefix + suffix visible — typical
# AKIA…XXXX pattern is identifiable; the body is masked. Set
# `redact: 0` to disable (PRINTS THE SECRET in logs — only do
# this in a private project) or `redact: 100` to fully mask.
cmd+=("--redact" "${REDACT}")

if [ -n "${PLUGIN_CONFIG:-}" ]; then
  cmd+=("--config" "${PLUGIN_CONFIG}")
fi

if [ -n "${PLUGIN_REPORT:-}" ]; then
  cmd+=("--report-path" "${PLUGIN_REPORT}")
fi

# Git 2.35 dubious-ownership workaround, same as every other
# plugin that touches git metadata.
git config --global --add safe.directory '*' 2>/dev/null || true

echo "==> gitleaks ${cmd[*]}"
exec gitleaks "${cmd[@]}"
