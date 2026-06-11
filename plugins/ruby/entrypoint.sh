#!/bin/sh
# gocdnext/ruby entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/ruby: $1" >&2; exit 2; }

[ -n "${PLUGIN_COMMAND:-}" ] || fail "PLUGIN_COMMAND is required (e.g. command: bundle exec rspec)"

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

git config --global --add safe.directory '*' 2>/dev/null || true

# Gems land inside the workspace so the native `cache:` block can
# tar them across runs (and the install-once-reuse-N artifact
# pattern works — vendor/bundle travels with the workspace).
export BUNDLE_PATH="${BUNDLE_PATH:-$(pwd)/vendor/bundle}"

NO_INSTALL="$(printf '%s' "${PLUGIN_NO_INSTALL:-false}" | tr '[:upper:]' '[:lower:]')"
case "${NO_INSTALL}" in
    true|1|yes|on)   NO_INSTALL=true ;;
    false|0|no|off)  NO_INSTALL=false ;;
    *) fail "no-install accepts true|false (got '${PLUGIN_NO_INSTALL}')" ;;
esac

if [ "${NO_INSTALL}" = "false" ] && [ -f Gemfile ]; then
    echo "==> bundle install (BUNDLE_PATH=${BUNDLE_PATH})"
    bundle install --quiet
elif [ "${NO_INSTALL}" = "true" ]; then
    echo "==> install skipped (no-install: true) — using the workspace's vendor/bundle as-is"
fi

echo "==> ${PLUGIN_COMMAND}"
exec sh -c "${PLUGIN_COMMAND}"
