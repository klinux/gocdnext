#!/bin/bash
# gocdnext/teams — post to a Microsoft Teams Incoming Webhook.
# See Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_WEBHOOK:-}" ]; then
    echo "gocdnext/teams: PLUGIN_WEBHOOK is required" >&2
    echo "  pipe via secrets: list to keep the URL out of logs" >&2
    exit 2
fi

case "${PLUGIN_WEBHOOK}" in
    *$'\n'*|*$'\r'*|*'"'*)
        echo "gocdnext/teams: webhook contains invalid characters" >&2
        exit 2
        ;;
esac

work="$(mktemp -d)"
chmod 700 "${work}"
trap 'rm -rf "${work}"' EXIT
printf 'url = "%s"\n' "${PLUGIN_WEBHOOK}" >"${work}/curl.conf"
chmod 600 "${work}/curl.conf"

if [ -n "${PLUGIN_PAYLOAD_FILE:-}" ]; then
    # Escape hatch: operator built their own Adaptive Card or
    # MessageCard JSON; we just forward it.
    body="${PLUGIN_PAYLOAD_FILE}"
    if [ ! -f "${body}" ]; then
        echo "gocdnext/teams: payload-file ${PLUGIN_PAYLOAD_FILE} not found" >&2
        exit 2
    fi
    echo "==> POST Teams webhook <-- ${body}"
    curl --config "${work}/curl.conf" -fSsL \
        -H "Content-Type: application/json" \
        -X POST \
        --data-binary "@${body}"
    exit $?
fi

# Status-tinted default theme color. Teams renders this as the
# left accent bar of the card — a quick red/green cue before the
# user reads the title.
theme="${PLUGIN_THEME_COLOR:-}"
if [ -z "${theme}" ]; then
    case "${CI_PIPELINE_STATUS:-unknown}" in
        success)  theme="2CBE4E" ;;  # emerald
        failed)   theme="CB2431" ;;  # red
        canceled) theme="F0B429" ;;  # amber
        *)        theme="6A737D" ;;  # muted gray
    esac
fi

title="${PLUGIN_TITLE:-${CI_PIPELINE:-run} #${CI_RUN_COUNTER:-?} → ${CI_PIPELINE_STATUS:-unknown}}"
message="${PLUGIN_MESSAGE:-commit ${CI_COMMIT_SHA:-?} on ${CI_COMMIT_BRANCH:-?}}"

# MessageCard is the simplest shape Incoming Webhooks still
# accept: @type/@context + themeColor + title + text. Build
# via jq so embedded quotes/newlines don't break the JSON.
payload=$(jq -nc \
    --arg type "MessageCard" \
    --arg context "https://schema.org/extensions" \
    --arg theme "${theme}" \
    --arg title "${title}" \
    --arg text "${message}" \
    '{
        "@type": $type,
        "@context": $context,
        themeColor: $theme,
        title: $title,
        text: $text
    }')

printf '%s' "${payload}" >"${work}/payload.json"
chmod 600 "${work}/payload.json"

echo "==> POST Teams webhook (${#title} + ${#message} chars)"
curl --config "${work}/curl.conf" -fSsL \
    -H "Content-Type: application/json" \
    -X POST \
    --data-binary "@${work}/payload.json"
