# gocdnext/matrix — post a message to a Matrix room via the
# client-server API. Uses PUT
# /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}
# so anything Matrix-compatible (Synapse, Conduit, Dendrite,
# matrix.org) accepts the call without a bridge.
#
# Auth model: an **access token** of a Matrix user the bot
# impersonates. Obtain once from the homeserver (login flow or
# the admin UI) and ship via `secrets:`. No OAuth dance — this
# is a machine-to-machine post, not a user session.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_HOMESERVER   (required)  Base URL (e.g. https://matrix.org
#                                   or https://chat.mycorp.com).
#   PLUGIN_TOKEN        (required)  Bearer access token; pipe via
#                                   secrets.
#   PLUGIN_ROOM_ID      (required)  Room id (!abc:server) OR alias
#                                   (#eng:server). Aliases get
#                                   resolved to ids before send.
#   PLUGIN_BODY         (optional)  Plain-text body. Defaults to
#                                   CI_* status line.
#   PLUGIN_HTML         (optional)  Rich HTML body. When set, the
#                                   message ships as m.text with
#                                   format: org.matrix.custom.html
#                                   and formatted_body = html.
#   PLUGIN_MSGTYPE      (optional)  "m.text" (default) or
#                                   "m.notice" — notice renders
#                                   grey in most clients, useful
#                                   for bot chatter vs real talk.
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-matrix
RUN chmod +x /usr/local/bin/gocdnext-matrix

ENTRYPOINT ["/usr/local/bin/gocdnext-matrix"]
