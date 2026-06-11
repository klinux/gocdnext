#!/bin/sh
# gocdnext/php entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/php: $1" >&2; exit 2; }

[ -n "${PLUGIN_COMMAND:-}" ] || fail "PLUGIN_COMMAND is required (e.g. command: vendor/bin/phpunit)"

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

git config --global --add safe.directory '*' 2>/dev/null || true

# Composer cache inside the workspace so the native `cache:` block
# can tar it across runs (same convention as go/python/dotnet).
export COMPOSER_CACHE_DIR="${COMPOSER_CACHE_DIR:-$(pwd)/.composer-cache}"
mkdir -p "${COMPOSER_CACHE_DIR}"

NO_INSTALL="$(printf '%s' "${PLUGIN_NO_INSTALL:-false}" | tr '[:upper:]' '[:lower:]')"
case "${NO_INSTALL}" in
    true|1|yes|on)   NO_INSTALL=true ;;
    false|0|no|off)  NO_INSTALL=false ;;
    *) fail "no-install accepts true|false (got '${PLUGIN_NO_INSTALL}')" ;;
esac

if [ "${NO_INSTALL}" = "false" ] && [ -f composer.json ]; then
    echo "==> composer install"
    # --no-interaction: CI never answers prompts. Lock-file when
    # present (composer respects it by default); --no-progress
    # keeps the job log scannable.
    composer install --no-interaction --no-progress
elif [ "${NO_INSTALL}" = "true" ]; then
    echo "==> install skipped (no-install: true) — using the workspace's vendor/ as-is"
fi

echo "==> ${PLUGIN_COMMAND}"
exec sh -c "${PLUGIN_COMMAND}"
