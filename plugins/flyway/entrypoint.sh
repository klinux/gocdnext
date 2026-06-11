#!/bin/sh
# gocdnext/flyway entrypoint — see Dockerfile for the contract.
#
# Connection material comes EXCLUSIVELY via env (FLYWAY_URL /
# FLYWAY_USER / FLYWAY_PASSWORD, populated by the job's `secrets:`
# list). There are no url/user/password inputs on purpose: `with:`
# values land in the persisted pipeline definition, and credentials
# must never live there.

set -eu

COMMAND="${PLUGIN_COMMAND:-}"
case "${COMMAND}" in
    info|validate|migrate) ;;
    "")
        echo "gocdnext/flyway: PLUGIN_COMMAND is required (info | validate | migrate)" >&2
        exit 2
        ;;
    repair)
        echo "gocdnext/flyway: 'repair' rewrites the schema history table and is not exposed —" >&2
        echo "  run it manually with operator context, then re-run the pipeline" >&2
        exit 2
        ;;
    *)
        echo "gocdnext/flyway: command must be info | validate | migrate (got '${COMMAND}')" >&2
        exit 2
        ;;
esac

if [ -z "${FLYWAY_URL:-}" ]; then
    echo "gocdnext/flyway: FLYWAY_URL env is required — populate via the job's secrets:" >&2
    echo "  secrets: [FLYWAY_URL, FLYWAY_USER, FLYWAY_PASSWORD]" >&2
    echo "  (Flyway reads its own FLYWAY_* env natively; values never touch argv)" >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

LOCATIONS="${PLUGIN_LOCATIONS:-filesystem:./migrations}"

# Postgres lock hygiene, ON BY DEFAULT — but the injected initSql
# (`SET lock_timeout/statement_timeout`) is POSTGRES SQL, and
# Flyway speaks to a dozen databases. Gate by URL: defaults apply
# only on jdbc:postgresql:; on other databases the defaults are
# skipped with a log line, and EXPLICIT lock-timeout /
# statement-timeout inputs fail loud instead of failing later at
# the tool with a cryptic SQL error. `init-sql` (full override) is
# always honoured — the operator owns it on any database.
#
# Why the default at all: a DDL that can't take its lock
# immediately queues — and every query behind it queues too, which
# is how a "tiny" ALTER TABLE takes prod down. lock_timeout makes
# the migration FAIL FAST; statement_timeout caps a runaway
# backfill. Set either input to "0" to disable.
case "${FLYWAY_URL}" in
    jdbc:postgresql:*) IS_PG=true ;;
    *)                 IS_PG=false ;;
esac

if [ "${IS_PG}" = "false" ] && { [ -n "${PLUGIN_LOCK_TIMEOUT:-}" ] || [ -n "${PLUGIN_STATEMENT_TIMEOUT:-}" ]; }; then
    echo "gocdnext/flyway: lock-timeout / statement-timeout inputs are Postgres-only" >&2
    echo "  (they inject 'SET lock_timeout' initSql, which is Postgres syntax)." >&2
    echo "  For other databases use init-sql with your engine's equivalent." >&2
    exit 2
fi

LOCK_TIMEOUT="${PLUGIN_LOCK_TIMEOUT:-5s}"
STATEMENT_TIMEOUT="${PLUGIN_STATEMENT_TIMEOUT:-15min}"
if [ -n "${PLUGIN_INIT_SQL:-}" ]; then
    export FLYWAY_INIT_SQL="${PLUGIN_INIT_SQL}"
elif [ -z "${FLYWAY_INIT_SQL:-}" ] && [ "${IS_PG}" = "true" ]; then
    init=""
    [ "${LOCK_TIMEOUT}" != "0" ] && init="SET lock_timeout = '${LOCK_TIMEOUT}';"
    [ "${STATEMENT_TIMEOUT}" != "0" ] && init="${init} SET statement_timeout = '${STATEMENT_TIMEOUT}';"
    if [ -n "${init}" ]; then
        export FLYWAY_INIT_SQL="${init}"
    fi
elif [ "${IS_PG}" = "false" ]; then
    echo "    non-Postgres URL: lock-hygiene initSql defaults skipped (Postgres-only syntax)"
fi

echo "==> flyway ${COMMAND} (locations=${LOCATIONS})"
if [ -n "${FLYWAY_INIT_SQL:-}" ]; then
    echo "    initSql: ${FLYWAY_INIT_SQL}"
fi

# -locations on argv is path config, not secret material. The
# connection trio stays in env where Flyway reads it natively.
exec flyway -locations="${LOCATIONS}" "${COMMAND}"
