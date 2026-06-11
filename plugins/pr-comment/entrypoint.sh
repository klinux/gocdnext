#!/bin/sh
# gocdnext/pr-comment entrypoint — see Dockerfile + plugin.yaml.
#
# Tokens come EXCLUSIVELY via env (the job's `secrets:` list) and
# ride curl's --header through a config file, never argv — argv is
# visible in `ps auxww` inside the container.

set -eu

fail() { echo "gocdnext/pr-comment: $1" >&2; exit 2; }

# --- trigger gate -------------------------------------------------
# Only PR/MR runs carry a comment target. Push/manual/tag runs are
# a SUCCESS no-op (loud), so a pipeline listening on
# `on: [push, pull_request]` doesn't fail its push leg.
if [ "${CI_CAUSE:-}" != "pull_request" ]; then
    echo "==> not a pull_request run (cause=${CI_CAUSE:-none}) — nothing to comment on, skipping"
    exit 0
fi
PR_URL="${CI_PULL_REQUEST_URL:-}"
[ -n "${PR_URL}" ] || fail "CI_PULL_REQUEST_URL is empty on a pull_request run — server older than v0.13?"

# --- body ---------------------------------------------------------
BODY_INPUT="${PLUGIN_BODY:-}"
BODY_FILE="${PLUGIN_BODY_FILE:-}"
if [ -n "${BODY_INPUT}" ] && [ -n "${BODY_FILE}" ]; then
    fail "set body: OR body-file:, not both"
fi
if [ -z "${BODY_INPUT}" ] && [ -z "${BODY_FILE}" ]; then
    fail "one of body: or body-file: is required"
fi
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
if [ -n "${BODY_FILE}" ]; then
    [ -f "${BODY_FILE}" ] || fail "body-file '${BODY_FILE}' not found in the workspace"
    cp "${BODY_FILE}" "${WORK}/body"
else
    printf '%s' "${BODY_INPUT}" > "${WORK}/body"
fi

# GitHub's comment cap is 65536 chars (the smallest of the three).
# Truncate at 60000 bytes with an explicit notice — a terraform
# plan that silently 422s helps nobody.
SIZE=$(wc -c < "${WORK}/body")
if [ "${SIZE}" -gt 60000 ]; then
    head -c 60000 "${WORK}/body" > "${WORK}/body.trunc"
    printf '\n\n_…truncated by gocdnext pr-comment (%s bytes over the 60000-byte cap; full output in the job log)_\n' "$((SIZE - 60000))" >> "${WORK}/body.trunc"
    mv "${WORK}/body.trunc" "${WORK}/body"
    echo "    body truncated: ${SIZE} bytes > 60000-byte cap"
fi

# --- upsert identity ----------------------------------------------
MODE="${PLUGIN_MODE:-upsert}"
case "${MODE}" in upsert|create) ;; *) fail "mode must be upsert or create (got '${MODE}')";; esac
MARKER="${PLUGIN_MARKER:-gocdnext/pr-comment:${CI_JOB_NAME:-job}}"
# Marker hygiene: '-->' would terminate the HTML comment early
# (breaking the sentinel AND letting a crafted marker forge other
# sentinels); a newline breaks single-line matching assumptions.
case "${MARKER}" in
    *"-->"*) fail "marker must not contain '-->'" ;;
esac
if [ "$(printf '%s' "${MARKER}" | wc -l)" -ne 0 ]; then
    fail "marker must be a single line"
fi
# The SENTINEL — the full '<!-- marker -->' string — is both what
# we append and what upsert searches for. Searching the bare
# marker would make prefixes collide ('plan' matching a comment
# carrying 'plan-extra'); the closing '-->' delimits exactly.
SENTINEL="<!-- ${MARKER} -->"
# Invisible in the rendered view on GitHub/GitLab; Bitbucket may
# render it as text (cosmetic, documented).
printf '\n\n%s\n' "${SENTINEL}" >> "${WORK}/body"
jq -Rs '{body: .}' < "${WORK}/body" > "${WORK}/payload.json"

# --- provider resolution from the PR URL --------------------------
# Path shape disambiguates, which also covers self-hosted GHE /
# GitLab (host is NOT github.com/gitlab.com there).
HOST_PATH="${PR_URL#*://}"
HOST="${HOST_PATH%%/*}"
URL_PATH="/${HOST_PATH#*/}"
PROVIDER="${PLUGIN_PROVIDER:-auto}"
if [ "${PROVIDER}" = "auto" ]; then
    case "${URL_PATH}" in
        */pull/*)           PROVIDER=github ;;
        */-/merge_requests/*) PROVIDER=gitlab ;;
        */pull-requests/*)  PROVIDER=bitbucket ;;
        *) fail "cannot detect provider from PR url path '${URL_PATH}' — set provider: explicitly" ;;
    esac
fi

API_BASE="${PLUGIN_API_BASE:-}"
AUTH_CFG="${WORK}/auth"   # curl --config file: headers stay off argv

case "${PROVIDER}" in
github)
    OWNER_REPO="${URL_PATH#/}"; OWNER_REPO="${OWNER_REPO%%/pull/*}"
    NUMBER="${URL_PATH##*/pull/}"; NUMBER="${NUMBER%%/*}"
    [ -n "${GITHUB_TOKEN:-}" ] || fail "GITHUB_TOKEN env is required for github (secrets: [GITHUB_TOKEN])"
    if [ -z "${API_BASE}" ]; then
        if [ "${HOST}" = "github.com" ]; then API_BASE="https://api.github.com"; else API_BASE="https://${HOST}/api/v3"; fi
    fi
    printf 'header = "Authorization: Bearer %s"\nheader = "Accept: application/vnd.github+json"\n' "${GITHUB_TOKEN}" > "${AUTH_CFG}"
    LIST_URL="${API_BASE}/repos/${OWNER_REPO}/issues/${NUMBER}/comments?per_page=100"
    CREATE_URL="${API_BASE}/repos/${OWNER_REPO}/issues/${NUMBER}/comments"
    UPDATE_URL_PREFIX="${API_BASE}/repos/${OWNER_REPO}/issues/comments/"   # + id, PATCH
    UPDATE_METHOD=PATCH
    FIND_FILTER='[.[] | select((.body // "") | contains($m))] | first | .id // empty'
    ;;
gitlab)
    PROJECT_PATH="${URL_PATH#/}"; PROJECT_PATH="${PROJECT_PATH%%/-/merge_requests/*}"
    NUMBER="${URL_PATH##*/-/merge_requests/}"; NUMBER="${NUMBER%%/*}"
    [ -n "${GITLAB_TOKEN:-}" ] || fail "GITLAB_TOKEN env is required for gitlab (secrets: [GITLAB_TOKEN])"
    [ -n "${API_BASE}" ] || API_BASE="https://${HOST}/api/v4"
    ENC_PATH=$(printf '%s' "${PROJECT_PATH}" | jq -Rr '@uri')
    printf 'header = "PRIVATE-TOKEN: %s"\n' "${GITLAB_TOKEN}" > "${AUTH_CFG}"
    LIST_URL="${API_BASE}/projects/${ENC_PATH}/merge_requests/${NUMBER}/notes?per_page=100"
    CREATE_URL="${API_BASE}/projects/${ENC_PATH}/merge_requests/${NUMBER}/notes"
    UPDATE_URL_PREFIX="${API_BASE}/projects/${ENC_PATH}/merge_requests/${NUMBER}/notes/"   # + id, PUT
    UPDATE_METHOD=PUT
    FIND_FILTER='[.[] | select((.body // "") | contains($m))] | first | .id // empty'
    ;;
bitbucket)
    WS_REPO="${URL_PATH#/}"; WS_REPO="${WS_REPO%%/pull-requests/*}"
    NUMBER="${URL_PATH##*/pull-requests/}"; NUMBER="${NUMBER%%/*}"
    [ -n "${API_BASE}" ] || API_BASE="https://api.bitbucket.org/2.0"
    if [ -n "${BITBUCKET_TOKEN:-}" ]; then
        printf 'header = "Authorization: Bearer %s"\n' "${BITBUCKET_TOKEN}" > "${AUTH_CFG}"
    elif [ -n "${BITBUCKET_USERNAME:-}" ] && [ -n "${BITBUCKET_APP_PASSWORD:-}" ]; then
        printf 'user = "%s:%s"\n' "${BITBUCKET_USERNAME}" "${BITBUCKET_APP_PASSWORD}" > "${AUTH_CFG}"
    else
        fail "bitbucket needs BITBUCKET_TOKEN or BITBUCKET_USERNAME+BITBUCKET_APP_PASSWORD via secrets:"
    fi
    LIST_URL="${API_BASE}/repositories/${WS_REPO}/pullrequests/${NUMBER}/comments?pagelen=100"
    CREATE_URL="${API_BASE}/repositories/${WS_REPO}/pullrequests/${NUMBER}/comments"
    UPDATE_URL_PREFIX="${API_BASE}/repositories/${WS_REPO}/pullrequests/${NUMBER}/comments/"   # + id, PUT
    UPDATE_METHOD=PUT
    # Bitbucket nests body under content.raw and paginates under .values.
    jq '{content: {raw: .body}}' < "${WORK}/payload.json" > "${WORK}/payload.bb.json" \
        && mv "${WORK}/payload.bb.json" "${WORK}/payload.json"
    FIND_FILTER='[.values[] | select((.content.raw // "") | contains($m))] | first | .id // empty'
    ;;
*)
    fail "provider must be auto | github | gitlab | bitbucket (got '${PROVIDER}')"
    ;;
esac

case "${NUMBER}" in (*[!0-9]*|"") fail "could not parse PR number from '${PR_URL}'";; esac

echo "==> pr-comment ${MODE} (${PROVIDER} #${NUMBER}, marker '${MARKER}')"

if [ "${PLUGIN_DRY_RUN:-false}" = "true" ]; then
    echo "    dry-run: create=${CREATE_URL}"
    echo "    dry-run: list=${LIST_URL}"
    echo "    dry-run: update=${UPDATE_URL_PREFIX}{id} (${UPDATE_METHOD})"
    echo "    dry-run: payload bytes=$(wc -c < "${WORK}/payload.json")"
    exit 0
fi

api() {
    # $1 method, $2 url, $3 optional payload file. Writes response
    # body to stdout; fails loud on HTTP >= 400 with the response
    # in the log (API error messages are how operators debug 403s).
    method="$1"; url="$2"; payload="${3:-}"
    set -- --silent --show-error --config "${AUTH_CFG}" \
        --header "Content-Type: application/json" \
        --request "${method}" --write-out '\n%{http_code}' "${url}"
    [ -n "${payload}" ] && set -- "$@" --data "@${payload}"
    resp=$(curl "$@") || fail "curl ${method} ${url} failed"
    code=$(printf '%s' "${resp}" | tail -n1)
    body=$(printf '%s' "${resp}" | sed '$d')
    if [ "${code}" -ge 400 ]; then
        echo "${body}" >&2
        fail "${PROVIDER} API ${method} returned HTTP ${code}"
    fi
    printf '%s' "${body}"
}

# find_existing walks ALL comment pages looking for the sentinel —
# a busy PR can hold the marker comment well past page 1, and a
# single-page lookup would stack a duplicate instead of editing.
# GitHub/GitLab paginate via &page=N (loop until a short/empty
# page); Bitbucket returns a `.next` URL to follow. The 50-page
# cap (5000 comments) is a runaway bound, and per the no-silent-
# caps rule it LOGS when hit — the worst case degrades to a
# duplicate comment, never a hang.
find_existing() {
    case "${PROVIDER}" in
    bitbucket)
        url="${LIST_URL}"
        pages=0
        while [ -n "${url}" ] && [ "${pages}" -lt 50 ]; do
            resp=$(api GET "${url}")
            id=$(printf '%s' "${resp}" | jq -r --arg m "${SENTINEL}" "${FIND_FILTER}")
            if [ -n "${id}" ]; then printf '%s' "${id}"; return 0; fi
            url=$(printf '%s' "${resp}" | jq -r '.next // empty')
            pages=$((pages + 1))
        done
        [ -n "${url}" ] && echo "    warn: stopped searching after 50 comment pages — may create a duplicate" >&2
        ;;
    *)
        page=1
        while [ "${page}" -le 50 ]; do
            resp=$(api GET "${LIST_URL}&page=${page}")
            count=$(printf '%s' "${resp}" | jq 'length')
            [ "${count}" -eq 0 ] && return 0
            id=$(printf '%s' "${resp}" | jq -r --arg m "${SENTINEL}" "${FIND_FILTER}")
            if [ -n "${id}" ]; then printf '%s' "${id}"; return 0; fi
            [ "${count}" -lt 100 ] && return 0
            page=$((page + 1))
        done
        echo "    warn: stopped searching after 50 comment pages — may create a duplicate" >&2
        ;;
    esac
    return 0
}

EXISTING_ID=""
if [ "${MODE}" = "upsert" ]; then
    EXISTING_ID=$(find_existing)
fi

if [ -n "${EXISTING_ID}" ]; then
    api "${UPDATE_METHOD}" "${UPDATE_URL_PREFIX}${EXISTING_ID}" "${WORK}/payload.json" > /dev/null
    echo "    updated existing comment ${EXISTING_ID}"
else
    api POST "${CREATE_URL}" "${WORK}/payload.json" > /dev/null
    echo "    created comment"
fi
