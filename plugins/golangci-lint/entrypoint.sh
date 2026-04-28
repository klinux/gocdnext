#!/bin/sh
# gocdnext/golangci-lint entrypoint — wraps `golangci-lint run`
# so pipelines hand it just the args, not the whole shell rune.
#
# Inputs (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_ARGS         (optional)  args after `run`. Default "./...".
#   PLUGIN_WORKING_DIR  (optional)  relative path under /workspace.
#   PLUGIN_TIMEOUT      (optional)  --timeout value. Default "5m".
#
# Exits with golangci-lint's own exit code so a failing CI step
# surfaces the lint issue directly.

set -eu

cd /workspace
if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — same workaround as the other
# plugins that touch git on the host-cloned workspace.
git config --global --add safe.directory '*' 2>/dev/null || true

# Cache golangci-lint's analysis cache inside the workspace so the
# platform's `cache:` block can tar it across runs. Default is
# $HOME/.cache/golangci-lint which sits outside the workspace.
export GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-/workspace/.golangci-cache}"
mkdir -p "${GOLANGCI_LINT_CACHE}"

TIMEOUT="${PLUGIN_TIMEOUT:-5m}"
ARGS="${PLUGIN_ARGS:-./...}"

# Banner so the log shows what's about to run — golangci-lint
# is silent when no issues are found AND stdout is a pipe, which
# makes a passing run indistinguishable from a hung one. Same
# pattern as the trivy plugin.
echo "==> golangci-lint run --timeout ${TIMEOUT} ${ARGS}"

# Word-split intentionally — a value like "--timeout 5m ./..."
# becomes three args. Operators with whitespace inside one arg
# should drop back to plain `script:`.
# shellcheck disable=SC2086
exec golangci-lint run --timeout "${TIMEOUT}" ${ARGS}
