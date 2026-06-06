#!/bin/bash
# gocdnext/ai-review — see Dockerfile for the full contract.

set -euo pipefail

# parse_bool — strict bool normaliser (same shape as go/maven/
# gradle/sonar plugins). Accepts true|false|1|0|yes|no|on|off
# case-insensitive. Empty = default.
parse_bool() {
    local name="$1"
    local val="$2"
    local default="$3"
    if [ -z "$val" ]; then
        printf '%s' "$default"
        return 0
    fi
    case "$(printf '%s' "$val" | tr '[:upper:]' '[:lower:]')" in
        true|1|yes|on)   printf 'true' ;;
        false|0|no|off)  printf 'false' ;;
        *)
            echo "gocdnext/ai-review: $name accepts true|false|1|0|yes|no|on|off (got '$val')" >&2
            exit 2
            ;;
    esac
}

# parse_int — positive integer with min bound. Typos like
# `max-diff-bytes: abc` would otherwise reach `head -c` and emit
# a cryptic "byte count required" error. Empty = default.
parse_int() {
    local name="$1"
    local val="$2"
    local default="$3"
    local min="${4:-0}"
    if [ -z "$val" ]; then
        printf '%s' "$default"
        return 0
    fi
    case "$val" in
        ''|*[!0-9]*)
            echo "gocdnext/ai-review: $name must be a non-negative integer (got '$val')" >&2
            exit 2
            ;;
    esac
    if [ "$val" -lt "$min" ]; then
        echo "gocdnext/ai-review: $name must be >= $min (got '$val')" >&2
        exit 2
    fi
    printf '%s' "$val"
}

# parse_float_0_to_1 — decimal in the closed range [0, 1]. Used
# for temperature; the OpenAI API rejects out-of-range with a
# cryptic 400. Catch early. Accepts integers (0, 1) and
# fractional values (0.7). Empty = default.
parse_float_0_to_1() {
    local name="$1"
    local val="$2"
    local default="$3"
    if [ -z "$val" ]; then
        printf '%s' "$default"
        return 0
    fi
    case "$val" in
        ''|*[!0-9.]*|*..*|.|..*)
            echo "gocdnext/ai-review: $name must be a decimal in [0, 1] (got '$val')" >&2
            exit 2
            ;;
    esac
    # Range check via awk to avoid spawning python/bc.
    if ! awk -v v="$val" 'BEGIN { if (v + 0 < 0 || v + 0 > 1) exit 1 }'; then
        echo "gocdnext/ai-review: $name must be in [0, 1] (got '$val')" >&2
        exit 2
    fi
    printf '%s' "$val"
}

# --- provider + creds -------------------------------------------
PROVIDER="$(printf '%s' "${PLUGIN_PROVIDER:-}" | tr '[:upper:]' '[:lower:]')"
case "${PROVIDER}" in
    claude)
        DEFAULT_MODEL="claude-sonnet-4-6"
        API_URL="https://api.anthropic.com/v1/messages"
        API_KEY="${ANTHROPIC_API_KEY:-}"
        if [ -z "${API_KEY}" ]; then
            echo "gocdnext/ai-review: ANTHROPIC_API_KEY is required for provider=claude" >&2
            echo "  pipe via the job's secrets: [ANTHROPIC_API_KEY]" >&2
            exit 2
        fi
        ;;
    openai)
        DEFAULT_MODEL="gpt-4o-mini"
        API_URL="https://api.openai.com/v1/chat/completions"
        API_KEY="${OPENAI_API_KEY:-}"
        if [ -z "${API_KEY}" ]; then
            echo "gocdnext/ai-review: OPENAI_API_KEY is required for provider=openai" >&2
            echo "  pipe via the job's secrets: [OPENAI_API_KEY]" >&2
            exit 2
        fi
        ;;
    "")
        echo "gocdnext/ai-review: provider: is required (claude | openai)" >&2
        exit 2
        ;;
    *)
        echo "gocdnext/ai-review: provider must be claude|openai (got '${PROVIDER}')" >&2
        exit 2
        ;;
esac

MODEL="${PLUGIN_MODEL:-${DEFAULT_MODEL}}"
BASE_REF="${PLUGIN_BASE_REF:-origin/main}"
HEAD_REF="${PLUGIN_HEAD_REF:-HEAD}"
MODE="${PLUGIN_MODE:-console}"
SEVERITY_THRESHOLD="${PLUGIN_SEVERITY_THRESHOLD:-warning}"
FAIL_ON_ERROR=$(parse_bool fail-on-error "${PLUGIN_FAIL_ON_ERROR:-}" "false") || exit $?
TEMPERATURE=$(parse_float_0_to_1 temperature "${PLUGIN_TEMPERATURE:-}" "0") || exit $?
MAX_TOKENS=$(parse_int max-tokens "${PLUGIN_MAX_TOKENS:-}" "4096" "1") || exit $?
MAX_DIFF_BYTES=$(parse_int max-diff-bytes "${PLUGIN_MAX_DIFF_BYTES:-}" "50000" "100") || exit $?

case "${MODE}" in
    console|pr-comment) ;;
    *)
        echo "gocdnext/ai-review: mode must be console|pr-comment (got '${MODE}')" >&2
        exit 2
        ;;
esac

case "${SEVERITY_THRESHOLD}" in
    info|warning|error) ;;
    *)
        echo "gocdnext/ai-review: severity-threshold must be info|warning|error (got '${SEVERITY_THRESHOLD}')" >&2
        exit 2
        ;;
esac

# --- pr-comment mode preconditions ------------------------------
if [ "${MODE}" = "pr-comment" ]; then
    if [ -z "${PLUGIN_PR_NUMBER:-}" ] || [ -z "${PLUGIN_REPO:-}" ]; then
        echo "gocdnext/ai-review: mode=pr-comment requires pr-number: and repo:" >&2
        exit 2
    fi
    SCM_PROVIDER="${PLUGIN_SCM_PROVIDER:-github}"
    case "${SCM_PROVIDER}" in
        github)
            if [ -z "${GITHUB_TOKEN:-}" ]; then
                echo "gocdnext/ai-review: GITHUB_TOKEN required for scm-provider=github + mode=pr-comment" >&2
                exit 2
            fi
            ;;
        gitlab)
            if [ -z "${GITLAB_TOKEN:-}" ]; then
                echo "gocdnext/ai-review: GITLAB_TOKEN required for scm-provider=gitlab + mode=pr-comment" >&2
                exit 2
            fi
            ;;
        *)
            echo "gocdnext/ai-review: scm-provider must be github|gitlab (got '${SCM_PROVIDER}')" >&2
            exit 2
            ;;
    esac
fi

# --- secrets-bearing scratch space ------------------------------
# All sensitive data (API keys via curl --config, SCM headers,
# request body) lands in a private tempdir wiped on exit. Avoids
# the "ps auxww" leak path that headers on the `curl -H ...` argv
# would otherwise create.
SCRATCH=$(mktemp -d)
chmod 700 "${SCRATCH}"
trap 'rm -rf "${SCRATCH}"' EXIT INT TERM

# --- compute diff -----------------------------------------------
git config --global --add safe.directory '*' 2>/dev/null || true

# The agent's clone may be shallow. Fetch enough history for the
# diff to resolve. Failure is non-fatal — `git diff` itself will
# surface a clearer error if the refs really aren't reachable.
git fetch origin --no-tags --depth=200 2>/dev/null || true

# Default exclude patterns target the high-noise / low-signal
# paths that dominate a typical PR diff: vendored deps, generated
# lock files, build outputs. Operator overrides via diff-exclude.
DIFF_EXCLUDE="${PLUGIN_DIFF_EXCLUDE:-}"
if [ -z "${DIFF_EXCLUDE}" ]; then
    DIFF_EXCLUDE=':(exclude)**/vendor/**
:(exclude)**/node_modules/**
:(exclude)**/*.lock
:(exclude)**/*.sum
:(exclude)**/dist/**
:(exclude)**/build/**'
fi

# Pathspec args — split on newline. `--` separates refs from
# pathspecs in `git diff`.
exclude_args=()
while IFS= read -r line; do
    [ -z "${line}" ] && continue
    exclude_args+=("${line}")
done <<< "${DIFF_EXCLUDE}"

# Compute diff. Three-dot syntax `BASE...HEAD` gives the diff of
# what HEAD adds since branching off BASE — the "PR view" of
# changes. Falls back to two-dot if three-dot doesn't resolve
# (detached HEAD scenarios).
#
# CRITICAL: distinguish "git diff failed" (refs unreachable —
# block the PR with rc=1) from "git diff returned empty" (refs
# reachable, no changes — happy path, rc=0). The previous
# conflation meant a fat-fingered `base-ref: origin/missing`
# silently bypassed the review.
DIFF=""
DIFF_OK=false
THREE_DOT_ERR=""
TWO_DOT_ERR=""

if THREE_DOT_OUT=$(git diff "${BASE_REF}...${HEAD_REF}" -- "${exclude_args[@]}" 2>&1); then
    DIFF="${THREE_DOT_OUT}"
    DIFF_OK=true
else
    THREE_DOT_ERR="${THREE_DOT_OUT}"
    if TWO_DOT_OUT=$(git diff "${BASE_REF}" "${HEAD_REF}" -- "${exclude_args[@]}" 2>&1); then
        DIFF="${TWO_DOT_OUT}"
        DIFF_OK=true
        echo "warn: fell back to two-dot diff (BASE..HEAD) — three-dot didn't resolve" >&2
    else
        TWO_DOT_ERR="${TWO_DOT_OUT}"
    fi
fi

if [ "${DIFF_OK}" != "true" ]; then
    echo "gocdnext/ai-review: git diff failed — refs unreachable?" >&2
    echo "  base=${BASE_REF} head=${HEAD_REF}" >&2
    echo "  three-dot: ${THREE_DOT_ERR:-<empty>}" >&2
    echo "  two-dot:   ${TWO_DOT_ERR:-<empty>}" >&2
    echo "  hint: agent's git clone may be shallow — bump fetch depth or check base-ref spelling" >&2
    exit 1
fi

if [ -z "${DIFF}" ]; then
    # Refs reachable, no changes since branching off base. Happy
    # path — no findings, no review needed.
    echo ":: empty diff — refs reachable but no changes to review" >&2
    mkdir -p .ai-review
    echo '[]' > .ai-review/findings.json
    exit 0
fi

# Truncate the diff at MAX_DIFF_BYTES to keep API costs bounded.
#
# Use bash's native substring expansion (`${var:offset:length}`)
# rather than `printf | head -c`. Under `set -euo pipefail`, the
# pipe would die with SIGPIPE when head closes early on a >50KB
# diff and the script would exit rc=1 BEFORE the API call —
# silently failing reviews on exactly the large PRs the truncate
# was supposed to cover. LC_ALL=C forces byte-level slicing so
# the bash semantics match the byte-based MAX_DIFF_BYTES limit
# (default alpine locale is POSIX/C anyway, this is defensive
# against base-image drift).
LC_ALL_SAVED="${LC_ALL:-}"
export LC_ALL=C
DIFF_BYTES=${#DIFF}
TRUNCATED=false
if [ "${DIFF_BYTES}" -gt "${MAX_DIFF_BYTES}" ]; then
    echo "warn: diff is ${DIFF_BYTES} bytes (truncating to ${MAX_DIFF_BYTES} for review)" >&2
    echo "  bump max-diff-bytes: to review larger PRs, but the LLM context window + cost grow linearly" >&2
    DIFF="${DIFF:0:${MAX_DIFF_BYTES}}"
    TRUNCATED=true
fi
export LC_ALL="${LC_ALL_SAVED}"

# --- system prompt ----------------------------------------------
DEFAULT_PROMPT='You are a senior code reviewer for a team practicing trunk-based development. Review the diff below.

Focus on (in priority order):
1. Correctness: bugs, off-by-one, nil/null handling, race conditions, missing error handling, edge cases.
2. Security: injection (SQL, command, template), auth bypass, secret leakage, OWASP top 10, supply-chain risks.
3. Performance: allocations in hot paths, N+1 queries, blocking calls in concurrent code, unbounded loops.
4. Maintainability: clarity, naming, complexity, dead code.

Output ONLY a JSON array of findings (no preamble, no markdown wrapper, no explanation outside JSON).

Each finding object MUST have:
- "severity": one of "info" | "warning" | "error"
- "file": relative path from repo root (as it appears in the diff)
- "line": integer line number in the NEW file, or null if not file-specific
- "title": one-line summary (max 100 chars)
- "description": multi-line explanation (root cause + impact)
- "suggestion": concrete code suggestion or remediation

Severity guidance:
- "error": definite bug, security issue, or contract violation.
- "warning": probable issue worth fixing before merge.
- "info": style or clarity nit.

If no issues are found, return [].'

PROMPT="${PLUGIN_SYSTEM_PROMPT:-${DEFAULT_PROMPT}}"

# --- save request + send ----------------------------------------
mkdir -p .ai-review
echo "==> ai-review provider=${PROVIDER} model=${MODEL} diff=${DIFF_BYTES}B$([ "${TRUNCATED}" = "true" ] && echo " (truncated)")"

CURL_CONFIG="${SCRATCH}/curl.conf"

case "${PROVIDER}" in
    claude)
        REQUEST=$(jq -n \
            --arg model "${MODEL}" \
            --argjson max_tokens "${MAX_TOKENS}" \
            --arg prompt "${PROMPT}" \
            --arg diff "${DIFF}" \
            '{
                model: $model,
                max_tokens: $max_tokens,
                system: $prompt,
                messages: [{
                    role: "user",
                    content: ("Review this diff:\n\n```diff\n" + $diff + "\n```\n\nRespond with ONLY the JSON array of findings.")
                }]
            }')
        printf '%s\n' "${REQUEST}" > .ai-review/request.json
        # Auth header lives in a 0600 config file inside $SCRATCH —
        # NOT on the curl argv (which is world-readable via
        # `ps auxww`). Config file is wiped by the EXIT trap.
        umask 077
        cat > "${CURL_CONFIG}" <<EOF
url = "${API_URL}"
request = "POST"
silent
show-error
fail-with-body
header = "x-api-key: ${API_KEY}"
header = "anthropic-version: 2023-06-01"
header = "content-type: application/json"
data = "@.ai-review/request.json"
EOF
        umask 022
        # if/then/else with the success branch on `then`. Capturing
        # `$?` inside `if ! cmd; then ...; fi` gives 0 because `!`
        # inverts the truthiness; we'd lose the real curl rc.
        if RESPONSE=$(curl --config "${CURL_CONFIG}"); then
            :
        else
            STATUS=$?
            echo "gocdnext/ai-review: Claude API call failed (curl rc=${STATUS})" >&2
            printf '%s\n' "${RESPONSE:-}" > .ai-review/error.txt
            exit 1
        fi
        printf '%s\n' "${RESPONSE}" > .ai-review/response.json
        TEXT=$(jq -r '.content[0].text // empty' .ai-review/response.json)
        ;;
    openai)
        REQUEST=$(jq -n \
            --arg model "${MODEL}" \
            --argjson temperature "${TEMPERATURE}" \
            --argjson max_tokens "${MAX_TOKENS}" \
            --arg prompt "${PROMPT}" \
            --arg diff "${DIFF}" \
            '{
                model: $model,
                temperature: $temperature,
                max_tokens: $max_tokens,
                response_format: {type: "json_object"},
                messages: [
                    {role: "system", content: ($prompt + "\n\nWrap the JSON array in a top-level object: {\"findings\": [...]}.")},
                    {role: "user", content: ("Review this diff:\n\n```diff\n" + $diff + "\n```")}
                ]
            }')
        printf '%s\n' "${REQUEST}" > .ai-review/request.json
        umask 077
        cat > "${CURL_CONFIG}" <<EOF
url = "${API_URL}"
request = "POST"
silent
show-error
fail-with-body
header = "Authorization: Bearer ${API_KEY}"
header = "content-type: application/json"
data = "@.ai-review/request.json"
EOF
        umask 022
        if RESPONSE=$(curl --config "${CURL_CONFIG}"); then
            :
        else
            STATUS=$?
            echo "gocdnext/ai-review: OpenAI API call failed (curl rc=${STATUS})" >&2
            printf '%s\n' "${RESPONSE:-}" > .ai-review/error.txt
            exit 1
        fi
        printf '%s\n' "${RESPONSE}" > .ai-review/response.json
        # OpenAI's response_format=json_object wraps the array;
        # the system prompt asks for {findings: [...]}.
        TEXT=$(jq -r '.choices[0].message.content // empty' .ai-review/response.json)
        # Unwrap the {findings: [...]} envelope to get a bare array.
        TEXT=$(printf '%s' "${TEXT}" | jq -c '.findings // .' 2>/dev/null || printf '%s' "${TEXT}")
        ;;
esac

# --- parse + filter findings ------------------------------------
if ! printf '%s' "${TEXT}" | jq -e 'type == "array"' >/dev/null 2>&1; then
    echo "gocdnext/ai-review: model returned non-array content" >&2
    echo "  raw response saved to .ai-review/response.json" >&2
    echo "  consider tightening the system prompt or upgrading the model" >&2
    printf '%s' "${TEXT}" > .ai-review/non-array.txt
    exit 1
fi

printf '%s' "${TEXT}" > .ai-review/findings.json

# Severity threshold filter: drop findings below the threshold.
case "${SEVERITY_THRESHOLD}" in
    info)    THRESHOLD_ARR='["info","warning","error"]' ;;
    warning) THRESHOLD_ARR='["warning","error"]' ;;
    error)   THRESHOLD_ARR='["error"]' ;;
esac
FILTERED=$(jq --argjson allowed "${THRESHOLD_ARR}" '[.[] | select(.severity as $s | $allowed | index($s))]' .ai-review/findings.json)
printf '%s' "${FILTERED}" > .ai-review/findings.filtered.json

FINDING_COUNT=$(jq 'length' .ai-review/findings.filtered.json)
ERROR_COUNT=$(jq '[.[] | select(.severity == "error")] | length' .ai-review/findings.filtered.json)
echo "==> findings: ${FINDING_COUNT} (severity >= ${SEVERITY_THRESHOLD}; ${ERROR_COUNT} error-severity)"

# --- output mode ------------------------------------------------
case "${MODE}" in
    console)
        if [ "${FINDING_COUNT}" -eq 0 ]; then
            echo ":: no findings at threshold ${SEVERITY_THRESHOLD}"
        else
            jq -r '.[] | "[\(.severity | ascii_upcase)] \(.file):\(.line // "?") — \(.title)\n    \(.description)\n    Suggest: \(.suggestion // "n/a")\n"' \
                .ai-review/findings.filtered.json
        fi
        ;;
    pr-comment)
        # Build markdown comment body.
        {
            echo "## :robot_face: AI review — ${PROVIDER} / ${MODEL}"
            echo
            if [ "${TRUNCATED}" = "true" ]; then
                echo "_⚠️ Diff truncated to ${MAX_DIFF_BYTES} bytes; review may miss issues in the tail. Bump \`max-diff-bytes\` or split the PR._"
                echo
            fi
            if [ "${FINDING_COUNT}" -eq 0 ]; then
                echo ":white_check_mark: No findings at severity threshold \`${SEVERITY_THRESHOLD}\`."
            else
                echo "Found **${FINDING_COUNT}** finding(s) at threshold \`${SEVERITY_THRESHOLD}\` (${ERROR_COUNT} error-severity)."
                echo
                jq -r '.[] |
                    "### " + ({"info":":information_source: INFO","warning":":warning: WARNING","error":":x: ERROR"}[.severity]) + " — `" + .file + ":" + (.line | tostring) + "`\n" +
                    "**" + .title + "**\n\n" +
                    .description + "\n\n" +
                    "<details><summary>Suggestion</summary>\n\n```\n" + (.suggestion // "n/a") + "\n```\n\n</details>\n"
                ' .ai-review/findings.filtered.json
            fi
            echo
            echo "_Generated by [gocdnext/ai-review](https://github.com/klinux/gocdnext/tree/main/plugins/ai-review). Audit trail: \`.ai-review/\` artifacts._"
        } > .ai-review/comment.md

        # SCM auth header goes in a 0600 config file inside
        # $SCRATCH so the token doesn't appear on the argv via
        # `ps auxww`. Wiped by the EXIT trap.
        SCM_CONFIG="${SCRATCH}/scm.conf"
        case "${SCM_PROVIDER}" in
            github)
                COMMENT_URL="https://api.github.com/repos/${PLUGIN_REPO}/issues/${PLUGIN_PR_NUMBER}/comments"
                jq -Rs '{body: .}' < .ai-review/comment.md > "${SCRATCH}/comment-body.json"
                umask 077
                cat > "${SCM_CONFIG}" <<EOF
url = "${COMMENT_URL}"
request = "POST"
silent
show-error
fail-with-body
header = "Authorization: Bearer ${GITHUB_TOKEN}"
header = "Accept: application/vnd.github+json"
header = "X-GitHub-Api-Version: 2022-11-28"
data = "@${SCRATCH}/comment-body.json"
EOF
                umask 022
                curl --config "${SCM_CONFIG}" > .ai-review/scm-response.json
                echo "==> posted GitHub PR comment"
                ;;
            gitlab)
                COMMENT_URL="https://gitlab.com/api/v4/projects/${PLUGIN_REPO}/merge_requests/${PLUGIN_PR_NUMBER}/notes"
                umask 077
                cat > "${SCM_CONFIG}" <<EOF
url = "${COMMENT_URL}"
request = "POST"
silent
show-error
fail-with-body
header = "PRIVATE-TOKEN: ${GITLAB_TOKEN}"
data-urlencode = "body@.ai-review/comment.md"
EOF
                umask 022
                curl --config "${SCM_CONFIG}" > .ai-review/scm-response.json
                echo "==> posted GitLab MR note"
                ;;
        esac
        ;;
esac

# --- exit gate --------------------------------------------------
if [ "${FAIL_ON_ERROR}" = "true" ] && [ "${ERROR_COUNT}" -gt 0 ]; then
    echo "gocdnext/ai-review: ${ERROR_COUNT} error-severity finding(s) — failing job (fail-on-error: true)" >&2
    exit 1
fi
exit 0
