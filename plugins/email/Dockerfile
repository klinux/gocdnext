# gocdnext/email — send an SMTP email. Most requested notifier
# after slack, especially for on-call / audit flows that already
# consume a shared mailbox.
#
# Uses Python 3's stdlib smtplib (no extra deps) so the image
# stays small and there's zero exposure to supply-chain risk
# from a notification lib. HTML vs plain text is chosen by the
# `format` input.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_HOST      (required)  SMTP host, e.g. "smtp.sendgrid.net".
#   PLUGIN_PORT      (optional)  port. Default 587 (STARTTLS).
#                                Set 465 for implicit-TLS.
#   PLUGIN_USERNAME  (optional)  auth user. Empty = skip auth
#                                (open relay inside a corp net).
#   PLUGIN_PASSWORD  (optional)  auth pass, piped via secrets.
#   PLUGIN_TLS       (optional)  "starttls" (default) | "tls" |
#                                "none". Match the port.
#   PLUGIN_FROM      (required)  From header, e.g.
#                                "ci@mycorp.com" or
#                                "CI <ci@mycorp.com>".
#   PLUGIN_TO        (required)  comma-separated recipients.
#   PLUGIN_CC        (optional)  comma-separated.
#   PLUGIN_SUBJECT   (required)  subject line.
#   PLUGIN_BODY      (required)  body text. `${CI_*}` variables
#                                in the YAML get expanded before
#                                this plugin runs.
#   PLUGIN_FORMAT    (optional)  "plain" (default) | "html".
FROM python:3.12-alpine

RUN apk add --no-cache bash ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-email
COPY send.py /usr/local/bin/gocdnext-email-send.py
RUN chmod +x /usr/local/bin/gocdnext-email /usr/local/bin/gocdnext-email-send.py

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-email"]
