#!/bin/bash
# gocdnext/coveralls — upload coverage to Coveralls. See
# Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_TOKEN:-}" ]; then
    echo "gocdnext/coveralls: token is required (pipe via secrets:)" >&2
    exit 2
fi

cd "/workspace/${PLUGIN_WORKING_DIR:-.}"

git config --global --add safe.directory '*' 2>/dev/null || true

file="${PLUGIN_FILE:-coverage/lcov.info}"
format="${PLUGIN_FORMAT:-lcov}"

if [ ! -f "${file}" ]; then
    echo "gocdnext/coveralls: coverage file ${file} not found (cwd=$(pwd))" >&2
    exit 2
fi

# Coveralls' node CLI reads its token from COVERALLS_REPO_TOKEN
# (or the older COVERALLS_SERVICE_NAME set pair). Set both so
# the CLI's internal service-detection doesn't complain.
export COVERALLS_REPO_TOKEN="${PLUGIN_TOKEN}"
export COVERALLS_SERVICE_NAME="${COVERALLS_SERVICE_NAME:-gocdnext}"
export COVERALLS_GIT_COMMIT="${CI_COMMIT_SHA:-}"
export COVERALLS_GIT_BRANCH="${CI_COMMIT_BRANCH:-}"
if [ -n "${CI_RUN_COUNTER:-}" ]; then
    export COVERALLS_SERVICE_JOB_ID="${CI_RUN_COUNTER}"
fi

if [ -n "${PLUGIN_BASE_URL:-}" ]; then
    export COVERALLS_ENDPOINT="${PLUGIN_BASE_URL%/}"
fi

if [ "${PLUGIN_PARALLEL:-false}" = "true" ]; then
    export COVERALLS_PARALLEL=true
fi

# coveralls-next supports LCOV natively; other formats land via
# --format. Finalise is a separate no-body POST; expose it as a
# plugin action so a pipeline's fan-in job can finish the
# parallel build without uploading another file.
if [ "${PLUGIN_FINALISE:-false}" = "true" ]; then
    # The "done" webhook Coveralls exposes for parallel builds.
    # POST to /webhook?repo_token=... with no body; curl is
    # lighter than pulling the CLI in for this one call.
    endpoint="${COVERALLS_ENDPOINT:-https://coveralls.io}/webhook"
    echo "==> POST ${endpoint} (finalise parallel)"
    exec curl -fSsL \
        -X POST \
        "${endpoint}?repo_token=${PLUGIN_TOKEN}&payload[build_num]=${CI_RUN_COUNTER:-0}&payload[status]=done"
fi

echo "==> coveralls-next --file ${file} --format ${format}"
exec coveralls-next --file "${file}" --format "${format}"
