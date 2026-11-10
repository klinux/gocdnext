#!/bin/bash
# gocdnext/playwright — Playwright end-to-end test runner. See
# Dockerfile for the full contract.

set -euo pipefail

cd "${PLUGIN_WORKING_DIR:-.}"

# Redirect the JUnit reporter output to a known path so the
# pipeline's `test_reports:` block can find it without the
# user having to sync two places. Respect an operator override
# when set — some CI configs pin the path explicitly.
export PLAYWRIGHT_JUNIT_OUTPUT_NAME="${PLAYWRIGHT_JUNIT_OUTPUT_NAME:-test-results/junit.xml}"

# The HTML reporter defaults to `open: 'on-failure'`, which starts
# `playwright show-report` as a blocking web server. In a non-interactive
# job container that never returns — the job hangs forever on the first
# failing test. Force the report to disk only; it's pulled as an artifact,
# never served. Overridable for the rare operator who sets it explicitly.
export PLAYWRIGHT_HTML_OPEN="${PLAYWRIGHT_HTML_OPEN:-never}"

# Detect the package manager from the lockfile on disk. Same
# priority GitHub Actions and Vercel use. Falls back to npm when a
# package.json exists without a lockfile. With no package.json at
# all it returns "none" and the caller skips dependency
# installation; the mock entrypoint_test uses that path to isolate
# argv dispatch from a real install.
detect_manager() {
    if [ -f "pnpm-lock.yaml" ]; then echo pnpm; return; fi
    if [ -f "yarn.lock" ]; then echo yarn; return; fi
    if [ -f "package-lock.json" ]; then echo npm; return; fi
    if [ -f "package.json" ]; then echo npm; return; fi
    echo none
}

manager="$(detect_manager)"

if [ "${PLUGIN_INSTALL_DEPS:-true}" = "true" ] && [ "${manager}" != "none" ]; then
    echo "==> installing dependencies (${manager})"
    case "${manager}" in
        pnpm) pnpm install --frozen-lockfile ;;
        yarn) yarn install --immutable 2>/dev/null || yarn install --frozen-lockfile ;;
        npm)
            if [ -f "package-lock.json" ]; then
                npm ci
            else
                npm install
            fi
            ;;
    esac
fi

# Assemble playwright flags. Everything below translates a
# PLUGIN_* input into the CLI's own flag; trailing EXTRA_ARGS
# get appended verbatim for anything the plugin didn't model.
args=()
if [ -n "${PLUGIN_CONFIG:-}" ]; then
    args+=(--config="${PLUGIN_CONFIG}")
fi
if [ -n "${PLUGIN_PROJECT:-}" ]; then
    args+=(--project="${PLUGIN_PROJECT}")
fi
if [ -n "${PLUGIN_GREP:-}" ]; then
    args+=(--grep="${PLUGIN_GREP}")
fi
if [ -n "${PLUGIN_SHARD:-}" ]; then
    args+=(--shard="${PLUGIN_SHARD}")
fi
if [ -n "${PLUGIN_REPORTER:-}" ]; then
    args+=(--reporter="${PLUGIN_REPORTER}")
else
    # Default combo: JUnit (for Tests tab ingestion) + HTML
    # (for the playwright-report/ artifact the user pulls
    # when debugging a failure).
    args+=(--reporter=junit,html)
fi

cmd="${PLUGIN_COMMAND:-test}"

# PLUGIN_EXTRA_ARGS is word-split intentionally so an operator can pass
# several flags in one input ("--workers=2 --retries=1"). But an UNQUOTED
# expansion also undergoes pathname (glob) expansion, so a `*`/`?`/`[…]` in
# the extra args — e.g. a test filter `tests/*.spec.ts` — would be silently
# rewritten by whatever files sit in the working directory before playwright
# sees it. `set -f` disables globbing while leaving word-splitting intact. The
# quoted "${cmd}" / "${args[@]}" are unaffected. (#361)
set -f

echo "==> npx playwright ${cmd} ${args[*]} ${PLUGIN_EXTRA_ARGS:-}"
# shellcheck disable=SC2086
exec npx playwright "${cmd}" "${args[@]}" ${PLUGIN_EXTRA_ARGS:-}
