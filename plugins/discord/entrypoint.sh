#!/bin/bash
# gocdnext/discord — post a message to a Discord webhook. See
# Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_WEBHOOK:-}" ]; then
    echo "gocdnext/discord: PLUGIN_WEBHOOK is required" >&2
    echo "  pipe via secrets: list to keep the URL out of logs" >&2
    exit 2
fi

case "${PLUGIN_WEBHOOK}" in
    *$'\n'*|*$'\r'*|*'"'*)
        echo "gocdnext/discord: webhook contains invalid characters" >&2
        exit 2
        ;;
esac

# Default content: "pipeline #N → status (sha)" — mirrors the
# slack plugin's default so a team running both receives the
# same shape on both channels.
if [ -z "${PLUGIN_CONTENT:-}" ]; then
    PLUGIN_CONTENT="${CI_PIPELINE:-run} #${CI_RUN_COUNTER:-?} → ${CI_PIPELINE_STATUS:-unknown} (${CI_COMMIT_SHA:-?})"
fi

# Build the JSON payload with jq so embedded quotes/newlines in
# the content string don't break the JSON.
payload=$(jq -nc \
    --arg content "${PLUGIN_CONTENT}" \
    --arg username "${PLUGIN_USERNAME:-}" \
    --arg avatar "${PLUGIN_AVATAR:-}" \
    --argjson tts "$([ "${PLUGIN_TTS:-false}" = "true" ] && echo true || echo false)" \
    '{
        content: $content,
        tts: $tts
    } + (if $username == "" then {} else {username: $username} end)
      + (if $avatar == "" then {} else {avatar_url: $avatar} end)')

# Keep the credential-bearing webhook URL and the message body out of process
# argv and logs. curl reads both from private files instead.
work="$(mktemp -d)"
chmod 700 "${work}"
trap 'rm -rf "${work}"' EXIT
printf 'url = "%s"\n' "${PLUGIN_WEBHOOK}" >"${work}/curl.conf"
printf '%s' "${payload}" >"${work}/payload.json"
chmod 600 "${work}/curl.conf" "${work}/payload.json"

# Discord returns 204 on success with no body; curl -f turns any 4xx/5xx into
# a non-zero exit so the plugin fails the job visibly.
echo "==> POST Discord webhook (${#PLUGIN_CONTENT} chars)"
curl --config "${work}/curl.conf" -fSsL \
    -H "Content-Type: application/json" \
    -X POST \
    --data-binary "@${work}/payload.json"
