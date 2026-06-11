# gocdnext/flyway — Flyway migration runner for gocdnext pipelines.
#
# Contract:
#   PLUGIN_COMMAND            (required)  info | validate | migrate.
#                                         `repair` is deliberately absent in
#                                         v1 — it rewrites the schema history
#                                         table and belongs to a human with
#                                         context, not a pipeline retry loop.
#   FLYWAY_URL                (required)  JDBC url, via the job's `secrets:`
#                                         when it embeds credentials.
#   FLYWAY_USER / FLYWAY_PASSWORD         via `secrets:` — Flyway reads its
#                                         own env natively; values never
#                                         touch argv (`ps auxww`-safe).
#   PLUGIN_LOCATIONS          (optional)  migrations dir, default
#                                         filesystem:./migrations
#   PLUGIN_LOCK_TIMEOUT       (optional)  default 5s — see entrypoint.
#   PLUGIN_STATEMENT_TIMEOUT  (optional)  default 15min.
#
# Official Flyway image, major-channel pin (same convention as the
# go plugin's golang:1.25). The OSS edition covers info/validate/
# migrate; dry-run ("check") is a Teams feature — the documented
# OSS preflight is validate+info on the PR pipeline.
FROM flyway/flyway:11-alpine

COPY entrypoint.sh /usr/local/bin/gocdnext-flyway
RUN chmod +x /usr/local/bin/gocdnext-flyway

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-flyway"]
