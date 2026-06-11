#!/bin/sh
# gocdnext/semgrep entrypoint — see Dockerfile for the contract.
#
# Design: semgrep always writes the FULL report (every severity) to
# the SARIF file; the fail-on threshold only decides the exit code,
# computed from the SARIF afterwards. Filtering the scan itself
# (--severity) would silently drop lower-severity findings from the
# artifact — gating and reporting are different concerns.

set -eu

fail() { echo "gocdnext/semgrep: $1" >&2; exit 2; }

FAIL_ON="$(printf '%s' "${PLUGIN_FAIL_ON:-error}" | tr '[:upper:]' '[:lower:]')"
case "${FAIL_ON}" in error|warning|info|none) ;; *) fail "fail-on must be error | warning | info | none (got '${FAIL_ON}')";; esac

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

CONFIG="${PLUGIN_CONFIG:-p/default}"
REPORT="${PLUGIN_REPORT_FILE:-semgrep.sarif}"

# Each comma/space-separated entry becomes its own --config flag —
# semgrep merges multiple rule sources natively.
set -- --sarif --output "${REPORT}" --metrics=off
old_ifs="${IFS}"; IFS=', '
for c in ${CONFIG}; do
    [ -n "${c}" ] && set -- "$@" --config "${c}"
done
IFS="${old_ifs}"
if [ -n "${PLUGIN_BASELINE_COMMIT:-}" ]; then
    set -- "$@" --baseline-commit "${PLUGIN_BASELINE_COMMIT}"
fi
# Explicit scan target — REQUIRED: without a positional path
# semgrep 1.x exits 2 printing only the metrics banner, a
# uniquely unhelpful failure mode. Default "." scans the
# working dir; `paths:` narrows it.
old_ifs="${IFS}"; IFS=', '
got_path=false
for p in ${PLUGIN_PATHS:-.}; do
    if [ -n "${p}" ]; then
        set -- "$@" "${p}"
        got_path=true
    fi
done
IFS="${old_ifs}"
[ "${got_path}" = "true" ] || set -- "$@" .

# Git 2.35+ dubious ownership — semgrep shells out to git for
# --baseline-commit and path filtering.
git config --global --add safe.directory '*' 2>/dev/null || true

echo "==> semgrep scan (config=${CONFIG} fail-on=${FAIL_ON} report=${REPORT})"

# Semgrep exit codes: 0 = clean OR findings (without --error),
# >=2 = scan errors (bad config, parse failures). We let scan
# errors propagate loud and own the findings-based exit below.
semgrep "$@"

# Severity gate from the SARIF. SARIF levels: error > warning >
# note (semgrep's INFO). Semgrep does NOT stamp .level on each
# result — per the SARIF spec the level defaults to the RULE's
# defaultConfiguration.level, so the gate resolves it via a
# ruleId→level lookup (spec fallback when both absent: warning).
# The artifact keeps everything regardless.
COUNTS=$(jq -r '
  [.runs[] as $r
   | ($r.tool.driver.rules // []
      | map({key: .id, value: (.defaultConfiguration.level // "warning")})
      | from_entries) as $lv
   | $r.results[]
   | (.level // $lv[.ruleId] // "warning")] as $all
  | [($all | map(select(. == "error"))   | length),
     ($all | map(select(. == "warning")) | length),
     ($all | length)]
  | @tsv' "${REPORT}")
ERRORS=$(printf '%s' "${COUNTS}" | cut -f1)
WARNINGS=$(printf '%s' "${COUNTS}" | cut -f2)
TOTAL=$(printf '%s' "${COUNTS}" | cut -f3)
NOTES=$((TOTAL - ERRORS - WARNINGS))
echo "    findings: ${TOTAL} (error=${ERRORS} warning=${WARNINGS} info=${NOTES}) — full report in ${REPORT}"

case "${FAIL_ON}" in
    none)    GATE=0 ;;
    error)   GATE="${ERRORS}" ;;
    warning) GATE=$((ERRORS + WARNINGS)) ;;
    info)    GATE="${TOTAL}" ;;
esac
if [ "${GATE}" -gt 0 ]; then
    echo "gocdnext/semgrep: ${GATE} finding(s) at or above '${FAIL_ON}' — failing the job" >&2
    exit 1
fi
