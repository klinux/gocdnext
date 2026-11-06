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
# Official Flyway image. Pinned to an EXACT patch + digest (not a floating
# :11/:13 tag) so a rebuild is reproducible and a CVE bump is a reviewable
# one-line change.
#
# MAJOR BUMP 11 -> 13 (CVE-driven): the latest 11.x (11.20.3) still shipped a
# fixable CRITICAL in its bundled io.netty:netty-handler 4.2.9 (CVE-2026-75595)
# that no 11.x image clears. 13.12.1 bundles netty-handler 4.2.19 (patched) and
# scans 0 fixable CRITICAL. Proven contract-safe: flyway 13 reads, validates and
# continues a schema-history table written by flyway 11 (no baseline reset) —
# see smoke_test.sh and the PR's upgrade spike. 13.12.1 is the current official
# release. Refresh patch + digest together to pull newer OS fixes.
#
# Runs as root (uid 0) — the upstream image's default, unchanged here (non-root
# is not this plugin's current contract). The OSS edition covers info/validate/
# migrate; dry-run ("check") is a Teams feature — the documented OSS preflight
# is validate+info on the PR pipeline.
FROM flyway/flyway:13.12.1-alpine@sha256:6af6ece9c5d46f62db7409dc19992d34886cd6376ddd81dbff23d5e03708cff9

COPY entrypoint.sh /usr/local/bin/gocdnext-flyway
RUN chmod +x /usr/local/bin/gocdnext-flyway

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-flyway"]
