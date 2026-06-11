#!/bin/sh
# gocdnext/pypi-publish entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/pypi-publish: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

DIST="${PLUGIN_DIST_DIR:-dist}"
[ -d "${DIST}" ] || fail "dist dir '${DIST}' not found — build in a prior job (python -m build) and ship dist/ via artifacts"
COUNT=$(find "${DIST}" -maxdepth 1 \( -name '*.whl' -o -name '*.tar.gz' \) | wc -l)
[ "${COUNT}" -gt 0 ] || fail "no .whl or .tar.gz in '${DIST}'"

CHECK_ONLY="$(printf '%s' "${PLUGIN_CHECK_ONLY:-false}" | tr '[:upper:]' '[:lower:]')"
echo "==> ${COUNT} distribution(s) in ${DIST}"

# twine check validates metadata + rendering — the no-token PR
# preflight (a broken long_description fails the upload AFTER the
# version is burned otherwise).
twine check "${DIST}"/*

if [ "${CHECK_ONLY}" = "true" ]; then
    echo "==> check-only: metadata valid, nothing uploaded"
    exit 0
fi

[ -n "${PYPI_TOKEN:-}" ] || fail "PYPI_TOKEN env is required (secrets: [PYPI_TOKEN])"
# Token auth via twine's env contract — never argv. __token__ is
# PyPI's literal username for API tokens.
export TWINE_USERNAME="__token__"
export TWINE_PASSWORD="${PYPI_TOKEN}"
export TWINE_NON_INTERACTIVE=1

set -- upload
if [ -n "${PLUGIN_REPOSITORY_URL:-}" ]; then
    set -- "$@" --repository-url "${PLUGIN_REPOSITORY_URL}"
fi
SKIP_EXISTING="$(printf '%s' "${PLUGIN_SKIP_EXISTING:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${SKIP_EXISTING}" = "true" ]; then
    # Files on PyPI are immutable — a retried pipeline that already
    # uploaded must not fail the run.
    set -- "$@" --skip-existing
fi

echo "==> twine upload (${PLUGIN_REPOSITORY_URL:-pypi.org}${SKIP_EXISTING:+, skip-existing})"
exec twine "$@" "${DIST}"/*
