#!/bin/bash
# gocdnext/github-release — publish a GitHub release. See
# Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_TAG:-}" ]; then
    echo "gocdnext/github-release: PLUGIN_TAG is required" >&2
    echo "  example: tag: v1.2.3" >&2
    exit 2
fi
if [ -z "${PLUGIN_TOKEN:-}" ]; then
    echo "gocdnext/github-release: PLUGIN_TOKEN is required" >&2
    echo "  pipe via secrets: list to keep plaintext out of logs" >&2
    exit 2
fi

# gh reads auth from GH_TOKEN. Exporting here (vs. passing
# --token on the CLI) keeps the token out of any argv trace the
# agent might log at debug level.
export GH_TOKEN="${PLUGIN_TOKEN}"

cd /workspace
git config --global --add safe.directory '*' 2>/dev/null || true

# Build the gh release create argv. gh is chatty about missing
# required flags so we don't reinvent the parser.
args=(release create "${PLUGIN_TAG}")

if [ -n "${PLUGIN_REPO:-}" ]; then
    args+=("--repo" "${PLUGIN_REPO}")
fi

if [ -n "${PLUGIN_TITLE:-}" ]; then
    args+=("--title" "${PLUGIN_TITLE}")
fi

# Notes source precedence: explicit PLUGIN_NOTES wins; if empty
# and generate-notes is true, let gh auto-generate from commit
# history since the previous tag.
if [ -n "${PLUGIN_NOTES:-}" ]; then
    args+=("--notes" "${PLUGIN_NOTES}")
elif [ "${PLUGIN_GENERATE_NOTES:-true}" = "true" ]; then
    args+=("--generate-notes")
else
    args+=("--notes" "")
fi

if [ "${PLUGIN_DRAFT:-false}" = "true" ]; then
    args+=("--draft")
fi

if [ "${PLUGIN_PRERELEASE:-false}" = "true" ]; then
    args+=("--prerelease")
fi

# Assets: comma- or newline-separated paths. gh accepts positional
# file args trailing the flags. Use a herestring so the read-loop
# runs in the same shell — piping into `while read` forks a
# subshell and the `args+=` appends there don't survive.
if [ -n "${PLUGIN_ASSETS:-}" ]; then
    assets=$(printf '%s' "${PLUGIN_ASSETS}" | tr ',' '\n')
    while IFS= read -r line; do
        line="${line## }"
        line="${line%% }"
        [ -z "${line}" ] && continue
        args+=("${line}")
    done <<<"${assets}"
fi

echo "==> gh ${args[*]}"
exec gh "${args[@]}"
