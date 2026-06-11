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
FROM liquibase/liquibase:4.31-alpine

COPY entrypoint.sh /usr/local/bin/gocdnext-liquibase
USER root
RUN chmod +x /usr/local/bin/gocdnext-liquibase
USER liquibase

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-liquibase"]
