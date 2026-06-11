#!/bin/sh
# gocdnext/osv-scanner entrypoint — see Dockerfile for the contract.
#
# Exit semantics: osv-scanner itself exits 1 when vulnerabilities
# are found and >1 on scan errors. fail-on=none converts ONLY the
# findings-exit (1) into success — scan errors stay loud, so a
# broken lockfile never masquerades as a clean report.

set -eu

fail() { echo "gocdnext/osv-scanner: $1" >&2; exit 2; }

FAIL_ON="$(printf '%s' "${PLUGIN_FAIL_ON:-any}" | tr '[:upper:]' '[:lower:]')"
case "${FAIL_ON}" in any|none) ;; *) fail "fail-on must be any | none (got '${FAIL_ON}')";; esac

FORMAT="$(printf '%s' "${PLUGIN_FORMAT:-sarif}" | tr '[:upper:]' '[:lower:]')"
case "${FORMAT}" in sarif|json|table) ;; *) fail "format must be sarif | json | table (got '${FORMAT}')";; esac

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

DIR="${PLUGIN_DIR:-.}"
[ -d "${DIR}" ] || fail "scan dir '${DIR}' not found in the workspace"
REPORT="${PLUGIN_REPORT_FILE:-osv-report.${FORMAT}}"

set -- scan source --recursive --format "${FORMAT}" --output "${REPORT}" "${DIR}"
if [ -n "${PLUGIN_CONFIG:-}" ]; then
    [ -f "${PLUGIN_CONFIG}" ] || fail "config '${PLUGIN_CONFIG}' not found in the workspace"
    set -- scan source --recursive --config "${PLUGIN_CONFIG}" --format "${FORMAT}" --output "${REPORT}" "${DIR}"
fi

echo "==> osv-scanner source scan (dir=${DIR} format=${FORMAT} fail-on=${FAIL_ON} report=${REPORT})"

RC=0
osv-scanner "$@" || RC=$?

# Findings summary for the job log (table format is already
# human-readable in the file; sarif/json get a jq count).
if [ -f "${REPORT}" ]; then
    case "${FORMAT}" in
        sarif) COUNT=$(jq '[.runs[].results[]] | length' "${REPORT}" 2>/dev/null || echo "?") ;;
        json)  COUNT=$(jq '[.results[].packages[].vulnerabilities[]] | length' "${REPORT}" 2>/dev/null || echo "?") ;;
        *)     COUNT="?" ;;
    esac
    echo "    findings: ${COUNT} — full report in ${REPORT}"
fi

case "${RC}" in
    0) exit 0 ;;
    1)
        if [ "${FAIL_ON}" = "none" ]; then
            echo "    vulnerabilities found, fail-on=none — reporting only"
            exit 0
        fi
        echo "gocdnext/osv-scanner: vulnerabilities found — failing the job (set fail-on: none for report-only)" >&2
        exit 1
        ;;
    *)
        # 128 = "no lockfiles found" in osv-scanner v2; everything
        # else is a scan error. Both stay loud regardless of
        # fail-on — silence here would fake a clean report.
        fail "scan error (exit ${RC}) — see log above"
        ;;
esac
