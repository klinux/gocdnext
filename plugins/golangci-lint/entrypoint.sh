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

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — same workaround as the other
# plugins that touch git on the host-cloned workspace.
git config --global --add safe.directory '*' 2>/dev/null || true

# Redirect every cache golangci-lint touches into the workspace so
# the platform's `cache:` block can tar them across runs:
#   GOLANGCI_LINT_CACHE — analysis cache (the linter's own).
#   GOMODCACHE          — fetched module archives. golangci-lint
#                         calls `go list` internally; without this
#                         the deps re-download every run.
#   GOCACHE             — compiled package artifacts. Lint passes
#                         need AST + type info, which require the
#                         compiler to do its work first; warm
#                         GOCACHE is the single biggest speed-up.
# Defaults sit under $HOME (/root/...) which is outside the
# workspace mount, so the cache block can't see them.
#
# ABSOLUTE paths, anchored to the CWD ($PWD is the working-dir after any
# PLUGIN_WORKING_DIR cd above): golangci-lint rejects a relative
# GOLANGCI_LINT_CACHE, and the `go list` it runs internally makes Go 1.26
# hard-reject a relative GOMODCACHE / GOCACHE ("must be absolute path") too.
# Normalise BOTH the default AND an explicit relative override — a
# `${VAR:-default}` only guards the UNSET case, so a relative value coming from
# variables:/profile/env would otherwise slip through. They still resolve to
# the same names under the (working-)dir, so the `cache:` paths are unchanged.
abs_under_cwd() {
    case "$1" in
        /*) printf '%s' "$1" ;;                # already absolute — keep
        *)  printf '%s/%s' "$(pwd)" "$1" ;;    # relative — anchor to CWD
    esac
}
GOLANGCI_LINT_CACHE="$(abs_under_cwd "${GOLANGCI_LINT_CACHE:-.golangci-cache}")"
GOMODCACHE="$(abs_under_cwd "${GOMODCACHE:-.go-mod}")"
GOCACHE="$(abs_under_cwd "${GOCACHE:-.go-cache}")"
export GOLANGCI_LINT_CACHE GOMODCACHE GOCACHE
mkdir -p "${GOLANGCI_LINT_CACHE}" "${GOMODCACHE}" "${GOCACHE}"

TIMEOUT="${PLUGIN_TIMEOUT:-5m}"
ARGS="${PLUGIN_ARGS:-./...}"

# Word-split intentionally — a value like "--timeout 5m ./..."
# becomes three args. Operators with whitespace inside one arg
# should drop back to plain `script:`. But an UNQUOTED expansion also
# undergoes pathname (glob) expansion, so a `*`/`?`/`[…]` in ARGS — e.g.
# a path filter `./pkg/*` — would be silently rewritten by whatever files
# sit in the working directory before golangci-lint sees it. `set -f`
# disables globbing while leaving word-splitting intact. (#361)
set -f

# Banner so the log shows what's about to run — golangci-lint
# is silent when no issues are found AND stdout is a pipe, which
# makes a passing run indistinguishable from a hung one. Same
# pattern as the trivy plugin.
echo "==> golangci-lint run --timeout ${TIMEOUT} ${ARGS}"

# shellcheck disable=SC2086
exec golangci-lint run --timeout "${TIMEOUT}" ${ARGS}
