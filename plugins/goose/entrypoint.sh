#!/bin/sh
# gocdnext/goose entrypoint — see Dockerfile for the contract.
#
# Connection material comes EXCLUSIVELY via env (GOOSE_DBSTRING,
# populated by the job's `secrets:` list — goose reads it
# natively). No dbstring input on purpose: `with:` values land in
# the persisted pipeline definition, and a DSN carries credentials.

set -eu

COMMAND="${PLUGIN_COMMAND:-}"
case "${COMMAND}" in
    status|validate|up) ;;
    "")
        echo "gocdnext/goose: PLUGIN_COMMAND is required (status | validate | up)" >&2
        exit 2
        ;;
    down|down-to|redo|reset)
        echo "gocdnext/goose: down/redo/reset are not exposed — migrations are forward-only" >&2
        echo "  in pipelines; ship a corrective migration instead" >&2
        echo "  (see the migrations concept page for why down-in-prod breaks canary)" >&2
        exit 2
        ;;
    *)
        echo "gocdnext/goose: command must be status | validate | up (got '${COMMAND}')" >&2
        exit 2
        ;;
esac

if [ -z "${GOOSE_DBSTRING:-}" ]; then
    echo "gocdnext/goose: GOOSE_DBSTRING env is required — populate via the job's secrets:" >&2
    echo "  secrets: [GOOSE_DBSTRING]" >&2
    echo "  (goose reads GOOSE_DBSTRING natively; the DSN never touches argv)" >&2
    echo "  Postgres lock hygiene goes in the DSN itself:" >&2
    echo "    postgres://u:p@db/app?options=-c%20lock_timeout%3D5s" >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

DRIVER="${PLUGIN_DRIVER:-postgres}"
export GOOSE_DRIVER="${DRIVER}"
DIR="${PLUGIN_DIR:-./migrations}"
if [ ! -d "${DIR}" ]; then
    echo "gocdnext/goose: migrations dir '${DIR}' not found in the workspace" >&2
    echo "  set with: { dir: path/to/migrations }" >&2
    exit 2
fi

echo "==> goose ${COMMAND} (driver=${DRIVER} dir=${DIR})"

# -dir on argv is path config, not secret material. The DSN stays
# in GOOSE_DBSTRING env where goose reads it natively.
exec goose -dir "${DIR}" "${COMMAND}"
