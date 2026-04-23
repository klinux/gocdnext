#!/bin/sh
# gocdnext/node entrypoint — thin shim around pnpm so pipelines don't
# have to hand-roll corepack/pnpm setup in every job's script.
#
# Inputs come as PLUGIN_* env vars (set by the agent from `with:`):
#   PLUGIN_COMMAND      (required)  pnpm subcommand + args, word-split.
#   PLUGIN_WORKING_DIR  (optional)  relative path under /workspace.
#
# Exits with pnpm's own exit code so CI reporting stays honest.

set -eu

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/node: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: install --frozen-lockfile" >&2
    echo "    command: test --run" >&2
    echo "    command: exec tsc --noEmit" >&2
    echo "    command: build" >&2
    exit 2
fi

# Land in the target directory — agent mounts the job workspace at
# /workspace, operator's `working-dir` is a path relative to that.
cd /workspace
if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" bites here because the host-cloned
# workspace is owned by the agent's UID while this container runs as
# root. Same workaround the docker engine applies for script tasks.
git config --global --add safe.directory '*' 2>/dev/null || true

# Let corepack resolve the pnpm version from the project's
# packageManager field. `--activate` forces the download now so the
# first `pnpm` call doesn't silently fail with a stale shim; see the
# ci-web `corepack prepare --activate` history for the motivating
# flake that drove this choice.
corepack enable
corepack prepare --activate

# Word-split intentionally — `install --frozen-lockfile` passes two
# args to pnpm. Operators with whitespace inside a single arg should
# lean on plain `script:` instead of this plugin.
# shellcheck disable=SC2086
exec pnpm ${PLUGIN_COMMAND}
