#!/bin/sh
# gocdnext/buf entrypoint — thin shim around the buf CLI so
# pipelines hand it just the subcommand + args.
#
# Inputs (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  buf subcommand + args, word-split.
#   PLUGIN_WORKING_DIR  (optional)  relative path under /workspace.
#
# Exits with buf's own exit code.

set -eu

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/buf: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: lint" >&2
    echo "    command: breaking --against .git#branch=main" >&2
    echo "    command: generate" >&2
    exit 2
fi

cd /workspace
if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — needed for `buf breaking
# --against .git#branch=main` to actually read the worktree.
git config --global --add safe.directory '*' 2>/dev/null || true

# Banner so the log shows what's about to run — `buf lint` on
# clean protos is silent, which reads like a hung job.
echo "==> buf ${PLUGIN_COMMAND}"

# Word-split intentionally: `breaking --against ref` is three
# args. Whitespace inside a single arg → drop to plain `script:`.
# shellcheck disable=SC2086
exec buf ${PLUGIN_COMMAND}
