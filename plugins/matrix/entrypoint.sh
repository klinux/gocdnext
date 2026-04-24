#!/bin/bash
# gocdnext/matrix — post a message to a Matrix room. See
# Dockerfile for the full contract.

set -euo pipefail

for var in PLUGIN_HOMESERVER PLUGIN_TOKEN PLUGIN_ROOM_ID; do
    if [ -z "${!var:-}" ]; then
        echo "gocdnext/matrix: ${var,,} is required" >&2
        exit 2
    fi
done

# Strip trailing slash from the homeserver so the URL join
# below doesn't produce a `//` segment the proxy might refuse.
homeserver="${PLUGIN_HOMESERVER%/}"
room="${PLUGIN_ROOM_ID}"
token="${PLUGIN_TOKEN}"

# Resolve room ALIAS (#eng:server) to a room ID (!abc:server).
# The PUT endpoint only accepts IDs; most operators keep the
# alias handy instead.
if [[ "${room}" == \#* ]]; then
    # URL-encode the # (and, while we're at it, the : just in
    # case — Matrix IDs use `:` which needs %3A in a URL path).
    encoded="${room//\#/%23}"
    encoded="${encoded//:/%3A}"
    resolved=$(curl -fSsL \
        -H "Authorization: Bearer ${token}" \
        "${homeserver}/_matrix/client/v3/directory/room/${encoded}" \
        | jq -r '.room_id // empty')
    if [ -z "${resolved}" ]; then
        echo "gocdnext/matrix: couldn't resolve alias ${room}" >&2
        exit 2
    fi
    room="${resolved}"
fi

encoded_room="${room//!/%21}"
encoded_room="${encoded_room//:/%3A}"

# txnId dedups retries — Matrix requires a unique-per-session id
# so a retried request doesn't double-post. Using nanoseconds +
# a random suffix is plenty for a single plugin invocation.
txn="gocdnext-$(date +%s%N)-${RANDOM}"

msgtype="${PLUGIN_MSGTYPE:-m.text}"
body="${PLUGIN_BODY:-${CI_PIPELINE:-run} #${CI_RUN_COUNTER:-?} → ${CI_PIPELINE_STATUS:-unknown} (${CI_COMMIT_SHA:-?})}"

if [ -n "${PLUGIN_HTML:-}" ]; then
    # Formatted body carries the HTML; Matrix clients fall back
    # to `body` on unsupported formats so we always include it.
    payload=$(jq -nc \
        --arg msgtype "${msgtype}" \
        --arg body "${body}" \
        --arg html "${PLUGIN_HTML}" \
        '{
            msgtype: $msgtype,
            body: $body,
            format: "org.matrix.custom.html",
            formatted_body: $html
        }')
else
    payload=$(jq -nc \
        --arg msgtype "${msgtype}" \
        --arg body "${body}" \
        '{msgtype: $msgtype, body: $body}')
fi

url="${homeserver}/_matrix/client/v3/rooms/${encoded_room}/send/m.room.message/${txn}"

echo "==> PUT ${url} (${#body} chars)"
curl -fSsL \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    -X PUT \
    -d "${payload}" \
    "${url}" >/dev/null
echo "==> delivered"
