#!/bin/bash
# gocdnext/coveralls — upload coverage to Coveralls. See
# plugin.yaml for the full contract.

set -euo pipefail

fail() {
    echo "gocdnext/coveralls: $1" >&2
    exit 2
}

[ -n "${PLUGIN_TOKEN:-}" ] || fail "token is required (pipe via secrets:)"

cd "${PLUGIN_WORKING_DIR:-.}"
git config --global --add safe.directory '*' 2>/dev/null || true

# The official reporter understands these variables directly. Keep the
# credential out of argv (/proc) and map gocdnext's generic run context to
# Coveralls' generic-CI model.
export COVERALLS_REPO_TOKEN="${PLUGIN_TOKEN}"
export COVERALLS_SERVICE_NAME="${COVERALLS_SERVICE_NAME:-gocdnext}"
export COVERALLS_GIT_COMMIT="${COVERALLS_GIT_COMMIT:-${CI_COMMIT_SHA:-}}"
export COVERALLS_GIT_BRANCH="${COVERALLS_GIT_BRANCH:-${CI_COMMIT_BRANCH:-}}"
if [ -n "${CI_RUN_COUNTER:-}" ]; then
    export COVERALLS_SERVICE_NUMBER="${COVERALLS_SERVICE_NUMBER:-${CI_RUN_COUNTER}}"
fi
if [ -n "${CI_RUN_ID:-}" ]; then
    export COVERALLS_SERVICE_JOB_ID="${COVERALLS_SERVICE_JOB_ID:-${CI_RUN_ID}}"
fi
if [ -n "${PLUGIN_BASE_URL:-}" ]; then
    export COVERALLS_ENDPOINT="${PLUGIN_BASE_URL%/}"
fi

parallel="${PLUGIN_PARALLEL:-false}"
finalise="${PLUGIN_FINALISE:-false}"
case "${parallel}" in true|false) ;; *) fail "parallel must be true or false (got '${parallel}')" ;; esac
case "${finalise}" in true|false) ;; *) fail "finalise must be true or false (got '${finalise}')" ;; esac

# A finalise job is deliberately entry-less: it closes the parallel build and
# must not require the default coverage/lcov.info to exist.
if [ "${finalise}" = "true" ]; then
    [ -n "${CI_RUN_COUNTER:-}" ] || fail "finalise requires CI_RUN_COUNTER to identify the parallel build"
    echo "==> coveralls done (build ${CI_RUN_COUNTER})"
    exec coveralls "done" --build-number="${CI_RUN_COUNTER}"
fi

file="${PLUGIN_FILE:-coverage/lcov.info}"
format="${PLUGIN_FORMAT:-lcov}"
[ -f "${file}" ] || fail "coverage file ${file} not found (cwd=$(pwd))"

# Preserve the plugin's historical `json` spelling while using the official
# reporter's name for Coveralls JSON. Reject typos before an upload attempt.
case "${format}" in
    json) reporter_format=coveralls ;;
    lcov|simplecov|cobertura) reporter_format="${format}" ;;
    *) fail "format must be lcov | json | simplecov | cobertura (got '${format}')" ;;
esac

args=(report "${file}" "--format=${reporter_format}")
if [ "${parallel}" = "true" ]; then
    [ -n "${CI_RUN_COUNTER:-}" ] || fail "parallel requires CI_RUN_COUNTER to identify the parallel build"
    args+=(--parallel "--build-number=${CI_RUN_COUNTER}")
fi

suffix=""
[ "${parallel}" = "true" ] && suffix=", parallel"
echo "==> coveralls report ${file} (format=${format}${suffix})"
exec coveralls "${args[@]}"
