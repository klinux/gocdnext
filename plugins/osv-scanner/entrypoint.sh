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

# fail-on-no-sources: by default a scan that finds NO package manifests is a
# clean pass (nothing to scan) — so a blanket _compliance_sca over a polyglot
# fleet doesn't fail on repos without a lockfile at the scanned path. Set true
# for a targeted scan that EXPECTS a lockfile and should fail if it's missing.
FAIL_NO_SOURCES="$(printf '%s' "${PLUGIN_FAIL_ON_NO_SOURCES:-false}" | tr '[:upper:]' '[:lower:]')"
case "${FAIL_NO_SOURCES}" in true|false) ;; *) fail "fail-on-no-sources must be true | false (got '${FAIL_NO_SOURCES}')";; esac

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
OSVLOG="$(mktemp)"
osv-scanner "$@" >"${OSVLOG}" 2>&1 || RC=$?
cat "${OSVLOG}"

# "No package sources found" is detected by MESSAGE, not just exit 128 — 128 is
# a general error code that can mean other failures too. Only the no-sources
# message gets the lenient (clean) treatment below.
NO_SOURCES=0
grep -qi 'no package sources found' "${OSVLOG}" && NO_SOURCES=1

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
        # No package sources found: nothing to scan. Clean pass by default
        # (a blanket _compliance_sca must not fail on a repo without a lockfile
        # at the scanned path); opt into failing with fail-on-no-sources: true.
        if [ "${NO_SOURCES}" = "1" ]; then
            if [ "${FAIL_NO_SOURCES}" = "true" ]; then
                fail "no package sources found in '${DIR}' (fail-on-no-sources=true)"
            fi
            echo "    no package sources found in '${DIR}' — nothing to scan (clean)"
            # Synthesize a parser-valid empty SARIF so the Security dashboard
            # records a clean SCA scan (clean != not-scanned).
            if [ "${FORMAT}" = "sarif" ] && [ ! -f "${REPORT}" ]; then
                printf '%s' '{"version":"2.1.0","$schema":"https://json.schemastore.org/sarif-2.1.0.json","runs":[{"tool":{"driver":{"name":"osv-scanner"}},"results":[]}]}' > "${REPORT}"
            fi
            exit 0
        fi
        # Any other non-{0,1} exit is a real scan error — stay loud, never fake
        # a clean report.
        fail "scan error (exit ${RC}) — see log above"
        ;;
esac
