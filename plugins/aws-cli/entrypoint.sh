#!/bin/bash
# gocdnext/aws-cli — thin wrapper around `aws`. See Dockerfile for
# the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/aws-cli: PLUGIN_COMMAND is required" >&2
    echo "  example: command: s3 cp dist/ s3://bkt/ --recursive" >&2
    exit 2
fi

WORKING_DIR="${PLUGIN_WORKING_DIR:-.}"
cd "/workspace/${WORKING_DIR}"

if [ -n "${PLUGIN_REGION:-}" ]; then
    export AWS_DEFAULT_REGION="${PLUGIN_REGION}"
fi

profile_args=()
if [ -n "${PLUGIN_PROFILE:-}" ]; then
    profile_args+=("--profile" "${PLUGIN_PROFILE}")
fi

# Word-splitting PLUGIN_COMMAND is intentional: users write
# "s3 cp a b --recursive" and expect the cli to see 4 tokens.
# shellcheck disable=SC2086
echo "==> aws ${PLUGIN_COMMAND}"
# shellcheck disable=SC2086
exec aws ${PLUGIN_COMMAND} "${profile_args[@]}"
