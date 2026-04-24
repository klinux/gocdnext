# gocdnext/playwright — run Playwright end-to-end tests with
# browsers pre-installed. Uses Microsoft's official image so
# Chromium/Firefox/WebKit don't have to be downloaded per run
# (~1.2 GB otherwise). Detects pnpm/yarn/npm from the lockfile,
# installs deps, then invokes `playwright test` with the
# operator's filters (project, grep, shard, config).
#
# Standard flow:
#   1. cd WORKING_DIR
#   2. install deps with the detected manager
#   3. npx playwright test [flags]
#   4. JUnit report lands at test-results/junit.xml by default
#      (env-var PLAYWRIGHT_JUNIT_OUTPUT_NAME) so the pipeline's
#      `test_reports:` block picks it up without extra config.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
#   PLUGIN_COMMAND      (optional)  Sub-command passed to
#                       `npx playwright`. Default "test". Useful
#                       overrides: "install" (no-op on this
#                       image but harmless), "show-report".
#   PLUGIN_CONFIG       (optional)  --config path override.
#                       Default reads playwright.config.{ts,js}
#                       from working-dir.
#   PLUGIN_PROJECT      (optional)  --project filter (single
#                       project from the config — chromium,
#                       firefox, mobile-chrome, …).
#   PLUGIN_GREP         (optional)  --grep <regex> to scope
#                       tests by title.
#   PLUGIN_SHARD        (optional)  --shard=N/M for parallel
#                       fan-out. "1/4" runs slice 1 of 4.
#   PLUGIN_REPORTER     (optional)  --reporter=<list>. Default
#                       "junit,html" — JUnit for the Tests tab
#                       + HTML for the playwright-report/
#                       artifact. "dot" is useful on a local
#                       inspector run.
#   PLUGIN_EXTRA_ARGS   (optional)  Freeform trailing flags
#                       forwarded verbatim. Escape hatch for
#                       knobs the plugin didn't model
#                       (--retries=2, --workers=4, …).
#   PLUGIN_INSTALL_DEPS (optional)  "true" (default) runs the
#                       detected manager's install step before
#                       tests. "false" skips it — use when a
#                       prior job already warmed node_modules
#                       (via gocdnext/node + `cache:`).
FROM mcr.microsoft.com/playwright:v1.50.0-noble

RUN apt-get update && apt-get install -y --no-install-recommends bash git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Image ships with Node 20 + npm. Enable corepack so pnpm + yarn
# resolve without a second `npm install -g`.
RUN corepack enable

COPY entrypoint.sh /usr/local/bin/gocdnext-playwright
RUN chmod +x /usr/local/bin/gocdnext-playwright

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-playwright"]
