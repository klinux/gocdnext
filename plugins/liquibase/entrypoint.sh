#!/bin/sh
# gocdnext/liquibase entrypoint — see Dockerfile for the contract.
#
# Connection material comes EXCLUSIVELY via env
# (LIQUIBASE_COMMAND_URL / _USERNAME / _PASSWORD, populated by the
# job's `secrets:` list). No url/user/password inputs on purpose:
# `with:` values land in the persisted pipeline definition, and
# credentials must never live there.

set -eu

COMMAND="${PLUGIN_COMMAND:-}"
case "${COMMAND}" in
    status|validate|update-sql|update) ;;
    "")
        echo "gocdnext/liquibase: PLUGIN_COMMAND is required (status | validate | update-sql | update)" >&2
        exit 2
        ;;
    rollback*)
        echo "gocdnext/liquibase: rollback commands are not exposed — migrations are" >&2
        echo "  forward-only in pipelines; ship a corrective changeset instead" >&2
        echo "  (see the migrations concept page for why down-in-prod breaks canary)" >&2
        exit 2
        ;;
    *)
        echo "gocdnext/liquibase: command must be status | validate | update-sql | update (got '${COMMAND}')" >&2
        exit 2
        ;;
esac

if [ -z "${LIQUIBASE_COMMAND_URL:-}" ]; then
    echo "gocdnext/liquibase: LIQUIBASE_COMMAND_URL env is required — populate via the job's secrets:" >&2
    echo "  secrets: [LIQUIBASE_COMMAND_URL, LIQUIBASE_COMMAND_USERNAME, LIQUIBASE_COMMAND_PASSWORD]" >&2
    echo "  (Liquibase reads its own LIQUIBASE_COMMAND_* env natively)" >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

CHANGELOG_FILE="${PLUGIN_CHANGELOG_FILE:-db/changelog/db.changelog-master.yaml}"
if [ ! -f "${CHANGELOG_FILE}" ]; then
    echo "gocdnext/liquibase: changelog file '${CHANGELOG_FILE}' not found in the workspace" >&2
    echo "  set with: { changelog-file: path/to/changelog.yaml }" >&2
    exit 2
fi

echo "==> liquibase ${COMMAND} (changelog=${CHANGELOG_FILE})"

# --changelog-file on argv is path config, not secret material.
# The connection trio stays in env where Liquibase reads it
# natively — never on argv.
exec liquibase --changelog-file="${CHANGELOG_FILE}" "${COMMAND}"
