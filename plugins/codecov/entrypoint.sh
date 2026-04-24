#!/bin/bash
# gocdnext/codecov — upload coverage to Codecov. See Dockerfile
# for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_TOKEN:-}" ]; then
    echo "gocdnext/codecov: token is required (pipe via secrets:)" >&2
    exit 2
fi

cd "/workspace/${PLUGIN_WORKING_DIR:-.}"

# Same dubious-ownership opt-in as every other git-aware plugin.
# codecov inspects the repo to derive the commit SHA + branch
# when they aren't passed explicitly.
git config --global --add safe.directory '*' 2>/dev/null || true

args=(upload-process --token "${PLUGIN_TOKEN}")

if [ -n "${PLUGIN_FILE:-}" ]; then
    args+=(--file "${PLUGIN_FILE}")
fi

if [ -n "${PLUGIN_FLAGS:-}" ]; then
    # --flag accepts one flag per invocation; split the comma
    # list into repeated --flag pairs so `unit,integration`
    # tags the upload with both.
    IFS=',' read -ra flags <<<"${PLUGIN_FLAGS}"
    for f in "${flags[@]}"; do
        f_trimmed="${f## }"
        f_trimmed="${f_trimmed%% }"
        [ -n "${f_trimmed}" ] && args+=(--flag "${f_trimmed}")
    done
fi

if [ -n "${PLUGIN_SLUG:-}" ]; then
    args+=(--slug "${PLUGIN_SLUG}")
fi

if [ -n "${PLUGIN_URL:-}" ]; then
    # Enterprise override — Codecov self-hosted targets a
    # different domain. The CLI reads --url as a full base.
    args+=(--url "${PLUGIN_URL}")
fi

# Hand over every gocdnext CI_* env var that Codecov knows how
# to read; the CLI auto-maps its own provider list but doesn't
# recognise gocdnext as a provider yet, so set the generic
# CODECOV_ENV vars to carry the commit + branch + build id.
export CODECOV_SHA="${CI_COMMIT_SHA:-}"
export CODECOV_BRANCH="${CI_COMMIT_BRANCH:-}"
export CODECOV_BUILD="${CI_RUN_COUNTER:-}"
export CODECOV_BUILD_URL="${CI_RUN_URL:-}"

echo "==> codecov ${args[*]}"
exec codecov "${args[@]}"
