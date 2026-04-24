# gocdnext/teams — post a message to a Microsoft Teams Incoming
# Webhook. Same philosophy as the slack/discord plugins: one HTTP
# POST with a MessageCard body, no SDK, no bot account needed.
#
# MessageCard is deprecated in favour of Adaptive Cards for NEW
# integrations as of 2025, but Incoming Webhooks still accept
# both. MessageCard is the smaller wire format + has zero
# schema-version drift, so this plugin emits that shape. An
# operator targeting a Power Automate workflow with Adaptive
# Cards can swap to `payload-file:` (override below).
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_WEBHOOK       (required)  Incoming Webhook URL. Populate
#                                    via secrets: — the URL
#                                    doubles as the auth token.
#   PLUGIN_TITLE         (optional)  Card title. Default is a
#                                    CI_* status line so a team
#                                    gets a meaningful default
#                                    without config.
#   PLUGIN_MESSAGE       (optional)  Body text (Markdown allowed).
#                                    Default summarises the run.
#   PLUGIN_THEME_COLOR   (optional)  6-digit hex without the #,
#                                    used as the card's accent
#                                    bar. Default auto-tints based
#                                    on CI_PIPELINE_STATUS: red on
#                                    failure, green on success,
#                                    gray otherwise.
#   PLUGIN_PAYLOAD_FILE  (optional)  Path (under workspace) to a
#                                    pre-built JSON body. Skips
#                                    every other input and POSTs
#                                    the file verbatim — the
#                                    escape hatch for operators
#                                    who want full control over
#                                    Adaptive Card content.
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-teams
RUN chmod +x /usr/local/bin/gocdnext-teams

ENTRYPOINT ["/usr/local/bin/gocdnext-teams"]
