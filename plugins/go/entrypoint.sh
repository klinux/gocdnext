#!/bin/sh
# gocdnext/go entrypoint — thin shim around `go` so pipelines
# don't hand-roll `apk add go && go build` in every job.
#
# Inputs (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  go subcommand + args, word-split.
#   PLUGIN_WORKING_DIR  (optional)  relative path under /workspace.
#
# Exits with the go CLI's own exit code.

set -eu

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/go: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: build ./..." >&2
    echo "    command: test -race ./..." >&2
    echo "    command: vet ./..." >&2
    exit 2
fi

cd /workspace
if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — same workaround as every other
# plugin that hits `git` on the host-cloned workspace.
git config --global --add safe.directory '*' 2>/dev/null || true

# Redirect both Go caches into the workspace so the platform's
# `cache:` block can tar them. GOMODCACHE holds fetched module
# archives (keyed by module@version — huge reuse win), GOCACHE
# holds compiled package artefacts (incremental builds + test
# result memoisation). Base image's defaults sit outside the
# workspace at /root/go + /root/.cache/go-build.
export GOMODCACHE="${GOMODCACHE:-/workspace/.go-mod}"
export GOCACHE="${GOCACHE:-/workspace/.go-cache}"
mkdir -p "${GOMODCACHE}" "${GOCACHE}"

# Banner so the log shows what's about to run — `go vet` / `go
# test` against a clean workspace can be silent for many seconds,
# and a silent log reads like the job is hung. Trivy follows the
# same convention.
echo "==> go ${PLUGIN_COMMAND}"

# Word-split intentionally: `build ./...` is two args. If an
# operator needs whitespace inside a single arg, drop back to
# plain `script:`; this plugin optimises the 90% case.
# shellcheck disable=SC2086
exec go ${PLUGIN_COMMAND}
