#!/bin/bash
# gocdnext/check-pipeline-run — preflight check against the gocdnext
# REST API. See Dockerfile for the full contract.

set -euo pipefail

API_URL="${PLUGIN_API_URL:-}"
API_TOKEN="${PLUGIN_API_TOKEN:-}"
PROJECT="${PLUGIN_PROJECT:-}"
PIPELINE="${PLUGIN_PIPELINE:-}"
TAG="${PLUGIN_TAG:-}"
REVISION="${PLUGIN_REVISION:-}"
EXPECTED_STATUS="${PLUGIN_EXPECTED_STATUS:-success}"
MAX_AGE="${PLUGIN_MAX_AGE:-7d}"
POLL_ATTEMPTS="${PLUGIN_POLL_ATTEMPTS:-1}"
POLL_INTERVAL="${PLUGIN_POLL_INTERVAL:-30}"
RUNS_LIMIT="${PLUGIN_RUNS_LIMIT:-100}"
OUTPUT="${PLUGIN_OUTPUT:-.gocdnext/preflight.env}"

# --- validate inputs --------------------------------------------------

if [ -z "${API_URL}" ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_API_URL is required" >&2
    exit 2
fi
if [ -z "${API_TOKEN}" ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_API_TOKEN is required (pipe via secrets:)" >&2
    exit 2
fi
# Token gets embedded in the curl --config quoted value below.
# A token containing `"`, `\`, or CR/LF would either break the
# config parse or inject extra header lines. Reject defensively
# — `gnk_*` tokens, JWTs, and base64 secrets all live in a safe
# subset; whitespace inside a bearer token is never legitimate.
# Don't echo the token (or any redacted form) in the message —
# even the length would leak structure.
if [[ "${API_TOKEN}" =~ [[:space:]\"\\] ]]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_API_TOKEN contains forbidden characters (whitespace, quote, or backslash)" >&2
    echo "  check the value in the corresponding secret — no value printed for security" >&2
    exit 2
fi
if [ -z "${PROJECT}" ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_PROJECT is required" >&2
    exit 2
fi
if [ -z "${PIPELINE}" ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_PIPELINE is required" >&2
    exit 2
fi

# Filter must be deterministic. A blank filter would match any
# recent success-of-named-pipeline run and defeat the point of a
# preflight that's supposed to confirm the specific tag passed.
if [ -z "${TAG}" ] && [ -z "${REVISION}" ]; then
    echo "gocdnext/check-pipeline-run: at least one of PLUGIN_TAG or PLUGIN_REVISION is required" >&2
    echo "  a blank filter would match any recent success and defeat the preflight" >&2
    exit 2
fi
# Enforce XOR — passing BOTH would silently AND the filters, which
# is a footgun: operator who set both probably wanted "tag exists
# OR revision exists", or just got confused. Force them to pick.
# Cross-checking a tag against its specific revision is a separate
# concern handled by the release pipeline upstream, not here.
if [ -n "${TAG}" ] && [ -n "${REVISION}" ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_TAG and PLUGIN_REVISION are mutually exclusive — pass exactly one" >&2
    echo "  got tag=${TAG} revision=${REVISION}" >&2
    exit 2
fi

# API URL: HTTPS recommended but HTTP allowed (dev / on-cluster).
# Reject shell metas + trailing slash so the appended path is
# predictable. Charset matches RFC 3986 for unreserved + commonly-
# accepted gen-delims, plus colon for `:port`.
api_url_re='^https?://[A-Za-z0-9._:-]+(/[A-Za-z0-9._/-]*)?$'
if ! [[ "${API_URL}" =~ ${api_url_re} ]]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_API_URL has forbidden characters or shape — got: ${API_URL}" >&2
    echo "  accepted: http(s)://host[:port][/path] with only [A-Za-z0-9._/-]" >&2
    exit 2
fi
# Strip any trailing slash so the appended /api/v1/... is clean.
API_URL="${API_URL%/}"

# Project/pipeline charsets: gocdnext's own parser allows the
# same characters (lowercase + digits + dash + underscore). Reject
# shell metas at the boundary.
name_re='^[A-Za-z0-9_.-]+$'
if ! [[ "${PROJECT}" =~ ${name_re} ]]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_PROJECT has forbidden characters — got: ${PROJECT}" >&2
    exit 2
fi
if ! [[ "${PIPELINE}" =~ ${name_re} ]]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_PIPELINE has forbidden characters — got: ${PIPELINE}" >&2
    exit 2
fi

# Tag charset: OCI tag spec — matches what gocdnext/image-copy
# emits, what gocdnext webhook stamps in cause_detail.tag_name.
# Allowing too broad (e.g. shell metas) would let the JSON
# comparison miss legit matches AND open a small injection
# surface in the jq filter below.
if [ -n "${TAG}" ]; then
    # Git refname-derived charset. Real-world Git tags include
    # `release/v1.2.3` (slash, namespacing convention) and
    # `v1.2.3+build.1` (SemVer build metadata), neither of which
    # are valid OCI image tags. cause_detail.tag_name comes from
    # the Git ref, not from the image tag, so OCI-only would
    # silently reject legit promotions.
    # Stays restrictive on shell metas ($, `, ;, &, |, <, >, (,
    # ), {, }, ', ", \, whitespace) and on Git refname forbidden
    # subsequences (`..`, `@{`, trailing `.` or `/`).
    tag_re='^[A-Za-z0-9_][A-Za-z0-9_./+-]*$'
    if ! [[ "${TAG}" =~ ${tag_re} ]]; then
        echo "gocdnext/check-pipeline-run: PLUGIN_TAG has forbidden characters — got: ${TAG}" >&2
        echo "  allowed: leading [A-Za-z0-9_], body [A-Za-z0-9_./+-]" >&2
        exit 2
    fi
    if [[ "${TAG}" == *..* ]] || [[ "${TAG}" == *"@{"* ]] || [[ "${TAG}" == */ ]] || [[ "${TAG}" == *. ]]; then
        echo "gocdnext/check-pipeline-run: PLUGIN_TAG contains forbidden Git refname subsequence (.., @{, trailing / or .) — got: ${TAG}" >&2
        exit 2
    fi
fi

if [ -n "${REVISION}" ]; then
    # SHA charset: hex, 7 to 40 chars. Accept prefixes so the
    # operator can pass `${CI_COMMIT_SHORT_SHA}` directly.
    rev_re='^[a-f0-9]{7,40}$'
    if ! [[ "${REVISION}" =~ ${rev_re} ]]; then
        echo "gocdnext/check-pipeline-run: PLUGIN_REVISION must be 7-40 hex chars — got: ${REVISION}" >&2
        exit 2
    fi
fi

# Expected status: comma-separated whitelist. Whitespace around
# each item is tolerated (`success, failed` is fine — humans copy
# from a doc and forget). Empty item or unknown status rejected.
IFS=',' read -r -a STATUS_ARR_RAW <<< "${EXPECTED_STATUS}"
STATUS_ARR=()
for s in "${STATUS_ARR_RAW[@]}"; do
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    if [ -z "${s}" ]; then
        echo "gocdnext/check-pipeline-run: PLUGIN_EXPECTED_STATUS has an empty item (stray comma?) — got: ${EXPECTED_STATUS}" >&2
        exit 2
    fi
    case "${s}" in
        success|failed|canceled) STATUS_ARR+=("${s}") ;;
        *)
            echo "gocdnext/check-pipeline-run: PLUGIN_EXPECTED_STATUS contains unknown status \"${s}\" — accepted: success, failed, canceled" >&2
            echo "  got: ${EXPECTED_STATUS}" >&2
            exit 2
            ;;
    esac
done

# Poll attempts: 1..60.
if ! [[ "${POLL_ATTEMPTS}" =~ ^[0-9]+$ ]] || [ "${POLL_ATTEMPTS}" -lt 1 ] || [ "${POLL_ATTEMPTS}" -gt 60 ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_POLL_ATTEMPTS must be integer in [1, 60] — got: ${POLL_ATTEMPTS}" >&2
    exit 2
fi

# Poll interval: positive integer seconds, cap 600.
if ! [[ "${POLL_INTERVAL}" =~ ^[0-9]+$ ]] || [ "${POLL_INTERVAL}" -lt 1 ] || [ "${POLL_INTERVAL}" -gt 600 ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_POLL_INTERVAL must be integer seconds in [1, 600] — got: ${POLL_INTERVAL}" >&2
    exit 2
fi

# Runs limit: how many recent runs of the project the plugin scans
# per attempt. Default + cap = 100 because the server-side handler
# (server/internal/api/projects/detail.go) silently caps `?runs=`
# at 100. Promising anything higher would lie about coverage and
# create false negatives for releases older than 100 runs. When
# raising the server cap is on the table, bump this cap and the
# error message in lockstep.
if ! [[ "${RUNS_LIMIT}" =~ ^[0-9]+$ ]] || [ "${RUNS_LIMIT}" -lt 1 ] || [ "${RUNS_LIMIT}" -gt 100 ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_RUNS_LIMIT must be integer in [1, 100] (server caps at 100) — got: ${RUNS_LIMIT}" >&2
    exit 2
fi

# Max age: <N>{m|h|d} or 0 to disable. Convert to seconds for the
# age comparison below.
MAX_AGE_SECONDS=0
if [ "${MAX_AGE}" != "0" ]; then
    if ! [[ "${MAX_AGE}" =~ ^([0-9]+)([mhd])$ ]]; then
        echo "gocdnext/check-pipeline-run: PLUGIN_MAX_AGE must match <N>{m|h|d} or 0 — got: ${MAX_AGE}" >&2
        exit 2
    fi
    n="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[2]}"
    case "${unit}" in
        m) MAX_AGE_SECONDS=$((n * 60)) ;;
        h) MAX_AGE_SECONDS=$((n * 3600)) ;;
        d) MAX_AGE_SECONDS=$((n * 86400)) ;;
    esac
fi

# Output path: workspace-relative, no absolute, no `..`. Same guard
# as semver-bump + image-copy.
if [ -z "${OUTPUT}" ]; then
    echo "gocdnext/check-pipeline-run: PLUGIN_OUTPUT must not be empty" >&2
    exit 2
fi
case "${OUTPUT}" in
    /*)
        echo "gocdnext/check-pipeline-run: PLUGIN_OUTPUT must be workspace-relative (no leading /) — got: ${OUTPUT}" >&2
        exit 2
        ;;
esac
case "${OUTPUT}" in
    ../*|*/../*|*/..)
        echo "gocdnext/check-pipeline-run: PLUGIN_OUTPUT must not traverse outside the workspace — got: ${OUTPUT}" >&2
        exit 2
        ;;
esac

# --- build curl --config file (token off argv) -----------------------

# Following the same pattern as gocdnext/ai-review, gocdnext/cosign,
# gocdnext/sonar — the bearer token NEVER appears on argv. mktemp
# 0600 + EXIT/INT/TERM trap so the file is wiped on every exit path
# including SIGINT during a slow API call.
curl_cfg=$(mktemp)
chmod 600 "${curl_cfg}"
trap 'rm -f "${curl_cfg}"' EXIT INT TERM
{
    echo 'silent'
    echo 'show-error'
    echo 'fail-with-body'
    echo "header = \"Authorization: Bearer ${API_TOKEN}\""
    echo 'header = "Accept: application/json"'
} > "${curl_cfg}"

# Inline `curl --config "${curl_cfg}"` so each call honours the
# auth + flags consistently.
api_get() {
    local path="$1"
    curl --config "${curl_cfg}" "${API_URL}${path}"
}

# --- helpers ----------------------------------------------------------

# parse_iso_to_epoch: turn an RFC 3339 timestamp from the API
# (e.g. 2026-06-07T00:00:00Z) into seconds since epoch. GNU
# coreutils `date` (apk's `coreutils` package above) handles
# this; BusyBox date would reject it.
parse_iso_to_epoch() {
    date --date="$1" '+%s' 2>/dev/null || echo 0
}

# now_epoch: current time in seconds since epoch. Wraps `date` so
# tests can override.
now_epoch() {
    date '+%s'
}

# match_summary: tests a candidate run summary (passed in $1, a
# JSON object) against pipeline + status + age. Emits the run ID
# on stdout when MATCH; returns 1 (skip, not anomalous) on
# pipeline/status/age mismatch; returns 2 (skip + anomalous —
# caller sets api_error_seen=true) when a candidate that DID
# match pipeline+status had unusable shape (missing/unparseable
# finished_at). Anomalous shape on a terminal run is API
# inconsistency, not "no match" — operator runbook should treat
# it as exit-3 territory, not exit-1.
match_summary() {
    local summary="$1"
    local pipeline_name status finished_at id
    pipeline_name=$(echo "${summary}" | jq -r '.pipeline_name')
    status=$(echo "${summary}" | jq -r '.status')
    finished_at=$(echo "${summary}" | jq -r '.finished_at // empty')
    id=$(echo "${summary}" | jq -r '.id')

    [ "${pipeline_name}" = "${PIPELINE}" ] || return 1

    local status_ok=false
    for s in "${STATUS_ARR[@]}"; do
        if [ "${s}" = "${status}" ]; then
            status_ok=true
            break
        fi
    done
    [ "${status_ok}" = "true" ] || return 1

    if [ "${MAX_AGE_SECONDS}" -gt 0 ]; then
        if [ -z "${finished_at}" ]; then
            echo "==> check-pipeline-run: run ${id} status=${status} but finished_at missing — treating as API shape anomaly" >&2
            return 2
        fi
        local finished_epoch now_e age
        finished_epoch=$(parse_iso_to_epoch "${finished_at}")
        now_e=$(now_epoch)
        if [ "${finished_epoch}" -eq 0 ]; then
            echo "==> check-pipeline-run: run ${id} finished_at=${finished_at} unparseable — treating as API shape anomaly" >&2
            return 2
        fi
        age=$((now_e - finished_epoch))
        if [ "${age}" -gt "${MAX_AGE_SECONDS}" ]; then
            return 1
        fi
    fi

    echo "${id}"
    return 0
}

# match_detail: given a run detail JSON, verifies the TAG /
# REVISION filter against cause_detail / revisions. Returns 0 on
# match.
match_detail() {
    local detail="$1"

    if [ -n "${TAG}" ]; then
        local tag_name
        tag_name=$(echo "${detail}" | jq -r '.cause_detail.tag_name // empty')
        [ "${tag_name}" = "${TAG}" ] || return 1
    fi

    if [ -n "${REVISION}" ]; then
        # Revisions is a JSONB map material_uuid → {revision, branch}.
        # We accept the FIRST revision (sorted material UUIDs, same
        # rule scheduler/civars.go::primaryRevision uses) so the
        # match is deterministic.
        local primary_rev
        primary_rev=$(echo "${detail}" | jq -r '
            .revisions
            | to_entries
            | sort_by(.key)
            | first.value.revision // empty
        ')
        case "${primary_rev}" in
            "${REVISION}"*) ;;
            *) return 1 ;;
        esac
    fi
    return 0
}

# extract_outputs: pull the fields we ship in the output file.
# Each field is validated against the shape the API contract says
# it MUST have — UUID, int counter, hex revision, RFC3339-ish
# timestamp. A malformed value here is either API corruption or a
# bug in this plugin's parser; either way, write-without-checking
# would shell-inject into a downstream `source preflight.env`.
extract_outputs() {
    local detail="$1"
    RUN_ID=$(echo "${detail}" | jq -r '.id')
    COUNTER=$(echo "${detail}" | jq -r '.counter')
    FINISHED_AT=$(echo "${detail}" | jq -r '.finished_at // empty')
    REVISION_OUT=$(echo "${detail}" | jq -r '
        .revisions
        | to_entries
        | sort_by(.key)
        | first.value.revision // empty
    ')

    if ! [[ "${RUN_ID}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; then
        echo "gocdnext/check-pipeline-run: API returned malformed run id (not a UUID): ${RUN_ID}" >&2
        exit 3
    fi
    if ! [[ "${COUNTER}" =~ ^[0-9]+$ ]]; then
        echo "gocdnext/check-pipeline-run: API returned malformed counter (not int): ${COUNTER}" >&2
        exit 3
    fi
    if [ -n "${REVISION_OUT}" ] && ! [[ "${REVISION_OUT}" =~ ^[a-f0-9]+$ ]]; then
        echo "gocdnext/check-pipeline-run: API returned malformed revision (not hex): ${REVISION_OUT}" >&2
        exit 3
    fi
    # Loose RFC3339 charset — covers `2026-06-07T18:29:19Z`,
    # `2026-06-07T18:29:19.123+00:00`, and trailing offsets. Empty
    # is fine (we only assert charset, not presence — presence is
    # enforced by match_summary when max-age is on).
    if [ -n "${FINISHED_AT}" ] && ! [[ "${FINISHED_AT}" =~ ^[0-9T:+.Z-]+$ ]]; then
        echo "gocdnext/check-pipeline-run: API returned malformed finished_at (not RFC3339): ${FINISHED_AT}" >&2
        exit 3
    fi

    RUN_URL="${API_URL}/runs/${RUN_ID}"
}

# --- main poll loop ---------------------------------------------------

attempt=0
matched=""
# Tracks whether ANY API/shape error happened during the poll.
# Without this, a 500/401 mid-poll could degrade into "no match"
# (exit 1) and the operator runbook would point at the upstream
# pipeline instead of the gocdnext API itself. We want "no match
# AND no errors" → 1, "errors occurred" → 3.
api_error_seen=false
while [ "${attempt}" -lt "${POLL_ATTEMPTS}" ]; do
    attempt=$((attempt + 1))
    echo "==> check-pipeline-run: attempt ${attempt}/${POLL_ATTEMPTS} (project=${PROJECT} pipeline=${PIPELINE} tag=${TAG:-<any>} revision=${REVISION:-<any>})"

    project_path="/api/v1/projects/${PROJECT}?runs=${RUNS_LIMIT}"
    if ! detail_json=$(api_get "${project_path}"); then
        echo "gocdnext/check-pipeline-run: API call failed against ${API_URL}${project_path}" >&2
        api_error_seen=true
        if [ "${attempt}" -ge "${POLL_ATTEMPTS}" ]; then
            break
        fi
        sleep "${POLL_INTERVAL}"
        continue
    fi

    # Validate the response shape BEFORE walking it. A `{"error":
    # "..."}` body that slipped past `fail-with-body` (e.g.
    # operator-facing 200 with body error in some proxies) would
    # otherwise produce `.runs | length == 0` and degrade into
    # silent no-match.
    if ! echo "${detail_json}" | jq -e '.runs | type == "array"' >/dev/null 2>&1; then
        echo "gocdnext/check-pipeline-run: unexpected API response shape from ${project_path} — expected .runs to be an array" >&2
        api_error_seen=true
        if [ "${attempt}" -ge "${POLL_ATTEMPTS}" ]; then
            break
        fi
        sleep "${POLL_INTERVAL}"
        continue
    fi

    # Iterate runs newest-first (the API already sorts by
    # created_at DESC). Pick the first that matches BOTH summary
    # filters AND (after a detail fetch) the tag/revision filter.
    runs_count=$(echo "${detail_json}" | jq -r '.runs | length')
    if [ "${runs_count}" = "0" ]; then
        echo "==> check-pipeline-run: project has no runs in window (limit=${RUNS_LIMIT})"
    else
        for i in $(seq 0 $((runs_count - 1))); do
            summary=$(echo "${detail_json}" | jq -r ".runs[${i}]")
            # Guard against `set -e` killing the script on the
            # non-zero return paths of match_summary. The `|| rc=$?`
            # construct is a conditional context, so set -e
            # passes through and we can inspect the actual code.
            ms_rc=0
            run_id=$(match_summary "${summary}") || ms_rc=$?
            case "${ms_rc}" in
                0) ;;
                2)
                    # Candidate matched pipeline+status but shape was
                    # anomalous (missing/unparseable finished_at).
                    # Track and keep scanning newer/older candidates.
                    api_error_seen=true
                    continue
                    ;;
                *) continue ;;
            esac

            # Fetch the run detail to verify tag/revision filter.
            if ! run_detail=$(api_get "/api/v1/runs/${run_id}?logs=0"); then
                # 401/500 here means we CANNOT distinguish a
                # legitimate-but-unreadable upstream from a
                # genuine no-match. Track and surface as exit 3.
                echo "gocdnext/check-pipeline-run: detail fetch failed for run ${run_id} — cannot verify filter" >&2
                api_error_seen=true
                continue
            fi
            # Detail shape: minimum fields the filter + extractor
            # rely on. A 200 with malformed body falls through to
            # api_error_seen so we surface exit 3 instead of "no
            # match".
            if ! echo "${run_detail}" | jq -e '
                (.id | type == "string")
                and (.counter | type == "number")
                and (.revisions | type == "object")
            ' >/dev/null 2>&1; then
                echo "gocdnext/check-pipeline-run: detail response for run ${run_id} has unexpected shape" >&2
                api_error_seen=true
                continue
            fi
            if match_detail "${run_detail}"; then
                matched="${run_detail}"
                break
            fi
        done
    fi

    if [ -n "${matched}" ]; then
        break
    fi

    if [ "${attempt}" -lt "${POLL_ATTEMPTS}" ]; then
        echo "==> check-pipeline-run: no match yet; sleeping ${POLL_INTERVAL}s"
        sleep "${POLL_INTERVAL}"
    fi
done

if [ -z "${matched}" ]; then
    if [ "${api_error_seen}" = "true" ]; then
        echo "gocdnext/check-pipeline-run: no match AND one or more API errors during the poll — exiting 3 (investigate API/network, not the upstream pipeline)" >&2
        echo "  project=${PROJECT} pipeline=${PIPELINE} tag=${TAG:-<any>} revision=${REVISION:-<any>} status=${EXPECTED_STATUS} max-age=${MAX_AGE}" >&2
        echo "  attempts=${POLL_ATTEMPTS} interval=${POLL_INTERVAL}s runs-limit=${RUNS_LIMIT}" >&2
        exit 3
    fi
    echo "gocdnext/check-pipeline-run: no run found matching the filter" >&2
    echo "  project=${PROJECT} pipeline=${PIPELINE} tag=${TAG:-<any>} revision=${REVISION:-<any>} status=${EXPECTED_STATUS} max-age=${MAX_AGE}" >&2
    echo "  attempts=${POLL_ATTEMPTS} interval=${POLL_INTERVAL}s runs-limit=${RUNS_LIMIT}" >&2
    echo "  if a match exists older than the last ${RUNS_LIMIT} runs of the project, tighten max-age or narrow the upstream pipeline name — the server caps scan window at 100 today" >&2
    exit 1
fi

# --- write output file -----------------------------------------------

extract_outputs "${matched}"

output_dir=$(dirname "${OUTPUT}")
if [ "${output_dir}" != "." ]; then
    mkdir -p "${output_dir}"
fi

# Path 1 (legacy, kept for backward compat).
{
    echo "# Generated by gocdnext/check-pipeline-run — do not edit."
    echo "RUN_ID='${RUN_ID}'"
    echo "RUN_URL='${RUN_URL}'"
    echo "REVISION='${REVISION_OUT}'"
    echo "COUNTER='${COUNTER}'"
    echo "FINISHED_AT='${FINISHED_AT}'"
} > "${OUTPUT}"

# Path 2 (native, gocdnext v0.11+): GOCDNEXT_OUTPUT_FILE — the
# agent-managed path. Operators declare `outputs:` and downstream
# references `${{ needs.preflight.outputs.run_id }}` etc.
if [ -n "${GOCDNEXT_OUTPUT_FILE:-}" ]; then
    {
        echo "RUN_ID=${RUN_ID}"
        echo "RUN_URL=${RUN_URL}"
        echo "REVISION=${REVISION_OUT}"
        echo "COUNTER=${COUNTER}"
        echo "FINISHED_AT=${FINISHED_AT}"
    } > "${GOCDNEXT_OUTPUT_FILE}"
fi

echo "==> check-pipeline-run: MATCHED"
echo "      RUN_ID      = ${RUN_ID}"
echo "      RUN_URL     = ${RUN_URL}"
echo "      REVISION    = ${REVISION_OUT}"
echo "      COUNTER     = ${COUNTER}"
echo "      FINISHED_AT = ${FINISHED_AT}"
echo "      written to: ${OUTPUT}"
