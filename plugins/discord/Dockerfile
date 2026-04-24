# gocdnext/discord — post a message to a Discord webhook.
# Sibling of gocdnext/slack; same idea, different wire format
# (Discord uses `content` + `username`/`avatar_url`; Slack uses
# `text` + `channel`).
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_WEBHOOK   (required)  Discord channel webhook URL.
#                                Pipe from `secrets:` list so
#                                the token stays out of logs.
#   PLUGIN_CONTENT   (optional)  Message body. Supports Discord
#                                markdown (bold, code, mentions).
#                                Empty = use a default CI_* status
#                                line, same as the slack plugin.
#   PLUGIN_USERNAME  (optional)  Override the bot display name
#                                for this message.
#   PLUGIN_AVATAR    (optional)  Override the bot avatar with
#                                an image URL.
#   PLUGIN_TTS       (optional)  "true" sends as text-to-speech
#                                (rarely useful in CI — default
#                                "false").
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-discord
RUN chmod +x /usr/local/bin/gocdnext-discord

ENTRYPOINT ["/usr/local/bin/gocdnext-discord"]
