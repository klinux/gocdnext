#!/bin/sh
# gocdnext/deploy-marker entrypoint — see Dockerfile.

set -eu

fail() { echo "gocdnext/deploy-marker: $1" >&2; exit 2; }

PROVIDER="$(printf '%s' "${PLUGIN_PROVIDER:-}" | tr '[:upper:]' '[:lower:]')"
[ -n "${PROVIDER}" ] || fail "provider: is required (datadog | grafana)"

TITLE="${PLUGIN_TITLE:-deploy: ${CI_PIPELINE_ID:+pipeline }${CI_TAG_NAME:-${CI_COMMIT_SHORT_SHA:-unknown}}}"
TEXT="${PLUGIN_TEXT:-Deployed ${CI_TAG_NAME:-${CI_COMMIT_SHA:-unknown}} (run ${CI_RUN_ID:-n/a})}"
TAGS="${PLUGIN_TAGS:-}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

post() {
    # $1 url, $2 payload file. Auth header file written by the
    # provider branch — token stays off argv.
    HTTP=$(curl --silent --show-error --config "${WORK}/auth" \
        --header "Content-Type: application/json" \
        --write-out '%{http_code}' --output "${WORK}/resp" \
        --data "@$2" "$1") || fail "request to $1 failed"
    if [ "${HTTP}" -ge 400 ]; then
        cat "${WORK}/resp" >&2
        fail "${PROVIDER} API returned HTTP ${HTTP}"
    fi
}

case "${PROVIDER}" in
datadog)
    [ -n "${DATADOG_API_KEY:-}" ] || fail "DATADOG_API_KEY env is required (secrets: [DATADOG_API_KEY])"
    API="${PLUGIN_API_BASE:-https://api.${DATADOG_SITE:-datadoghq.com}}"
    printf 'header = "DD-API-KEY: %s"\n' "${DATADOG_API_KEY}" > "${WORK}/auth"
    # tags: "env:prod,service:shop" → JSON array
    jq -n --arg title "${TITLE}" --arg text "${TEXT}" --arg tags "${TAGS}" \
        '{title: $title, text: $text, source_type_name: "gocdnext",
          tags: ($tags | split(",") | map(select(length > 0)))}' > "${WORK}/payload"
    echo "==> datadog event: ${TITLE}"
    post "${API}/api/v1/events" "${WORK}/payload"
    ;;
grafana)
    [ -n "${GRAFANA_TOKEN:-}" ] || fail "GRAFANA_TOKEN env is required (secrets: [GRAFANA_TOKEN])"
    GRAFANA_URL="${PLUGIN_API_BASE:-${PLUGIN_GRAFANA_URL:-${GRAFANA_URL:-}}}"
    [ -n "${GRAFANA_URL}" ] || fail "grafana-url: is required (your Grafana base URL)"
    printf 'header = "Authorization: Bearer %s"\n' "${GRAFANA_TOKEN}" > "${WORK}/auth"
    jq -n --arg text "${TITLE} — ${TEXT}" --arg tags "${TAGS}" \
        '{text: $text, tags: (($tags | split(",") | map(select(length > 0))) + ["deploy","gocdnext"])}' > "${WORK}/payload"
    echo "==> grafana annotation: ${TITLE}"
    post "${GRAFANA_URL%/}/api/annotations" "${WORK}/payload"
    ;;
*)
    fail "provider must be datadog | grafana (got '${PROVIDER}')"
    ;;
esac
echo "    marker recorded"
