#!/bin/sh
# gocdnext/crates-publish entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/crates-publish: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi
DIR="${PLUGIN_DIR:-.}"
[ -f "${DIR}/Cargo.toml" ] || fail "no Cargo.toml in '${DIR}'"

git config --global --add safe.directory '*' 2>/dev/null || true

DRY_RUN="$(printf '%s' "${PLUGIN_DRY_RUN:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${DRY_RUN}" != "true" ]; then
    # cargo reads CARGO_REGISTRY_TOKEN from env natively — the
    # token never touches argv.
    [ -n "${CARGO_REGISTRY_TOKEN:-}" ] || fail "CARGO_REGISTRY_TOKEN env is required (secrets: [CARGO_REGISTRY_TOKEN])"
fi

set -- publish
[ "${DRY_RUN}" = "true" ] && set -- "$@" --dry-run
ALLOW_DIRTY="$(printf '%s' "${PLUGIN_ALLOW_DIRTY:-false}" | tr '[:upper:]' '[:lower:]')"
[ "${ALLOW_DIRTY}" = "true" ] && set -- "$@" --allow-dirty

echo "==> cargo publish (dir=${DIR}${DRY_RUN:+ dry-run=${DRY_RUN}})"
cd "${DIR}"
exec cargo "$@"
