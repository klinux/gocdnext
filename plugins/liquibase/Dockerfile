# gocdnext/liquibase — Liquibase migration runner for gocdnext
# pipelines.
#
# Contract:
#   PLUGIN_COMMAND               (required)  status | validate |
#                                            update-sql | update.
#   LIQUIBASE_COMMAND_URL        (required)  JDBC url via `secrets:`.
#   LIQUIBASE_COMMAND_USERNAME / LIQUIBASE_COMMAND_PASSWORD
#                                            via `secrets:` — Liquibase
#                                            reads its own env natively.
#   PLUGIN_CHANGELOG_FILE        (optional)  default
#                                            db/changelog/db.changelog-master.yaml
#
# `update-sql` is the REAL dry-run: prints the SQL that `update`
# would execute, without touching the database — run it on PR
# pipelines so the reviewer sees the exact DDL in the job log.
#
# Official image, minor-channel pin. Liquibase has no initSql
# equivalent — Postgres lock hygiene goes through the JDBC url's
# options parameter instead (documented in the plugin examples):
#   jdbc:postgresql://host/db?options=-c%20lock_timeout%3D5s
# Pinned by digest (4.31.1-alpine). The liquibase VERSION is deliberately NOT
# bumped: this is a schema-migration tool, and the two CRITICALs are cleared
# without touching liquibase itself, so its SQL/changelog behaviour is unchanged.
#   - OS OpenSSL (libcrypto3/libssl3 CVE-2026-31789): `apk upgrade`.
#   - `/liquibase/bin/lpm` (the Liquibase Package Manager, a Go binary on an old
#     Go → stdlib CVE-2025-68121): REMOVED. The plugin only ever runs `liquibase`
#     (status/validate/update-sql/update); it never invokes lpm, which is a
#     separate CLI for installing extensions.
# apk upgrade pulls Alpine's mutable repos; the FROM digest pins the base layer,
# the weekly Trivy gate catches drift.
FROM liquibase/liquibase:4.31-alpine@sha256:d7e17e2d234835fe132c4c46ec1af7d7d490710e882db162abb15593c4948fe8

COPY entrypoint.sh /usr/local/bin/gocdnext-liquibase
USER root
RUN apk upgrade --no-cache \
 && rm -f /liquibase/bin/lpm \
 && chmod +x /usr/local/bin/gocdnext-liquibase
USER liquibase

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-liquibase"]
