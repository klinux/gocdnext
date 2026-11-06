# gocdnext/netlify — deploy a built directory to Netlify.
#
# Contract:
#   NETLIFY_AUTH_TOKEN (required)  via `secrets:` — the CLI reads
#                                  it natively from env.
#   PLUGIN_DIR         (required)  the built site directory.
#   PLUGIN_SITE_ID     (required)  Netlify site (or set
#                                  NETLIFY_SITE_ID via secrets).
#   PLUGIN_PROD        (optional)  production deploy vs preview.
#   PLUGIN_ALIAS       (optional)  preview alias (pr-N etc).
#
# DEPLOY-ONLY image. It exposes `netlify deploy --no-build` exclusively;
# `netlify dev` / local function emulation are NOT supported. That lets us drop
# the bundled `@netlify/local-functions-proxy` Go executable (used only by the
# local-dev proxy), which ships old Go-stdlib CVEs with no upstream fix — 27.16.2
# is the latest CLI. The JS wrapper is kept; only the standalone binary goes.
#
# Node is pinned by exact patch + digest; netlify-cli by exact version. The CLI
# jump 17 → 27 is a major-version change and may be breaking for a pipeline.
FROM node:24.22.2-alpine@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8

# Install the CLI, then remove ONLY the local-functions-proxy executable(s).
# Fail the build if the binary isn't where we expect (a layout change must not
# silently skip the hardening) and verify none remain afterward.
RUN npm install -g netlify-cli@27.16.2 && npm cache clean --force \
    && nm=/usr/local/lib/node_modules/netlify-cli/node_modules \
    && found="$(find "$nm" -type f -path '*/@netlify/local-functions-proxy-*/bin/local-functions-proxy')" \
    && [ -n "$found" ] || { echo 'FATAL: local-functions-proxy binary not found — netlify-cli layout changed, refusing to ship un-hardened' >&2; exit 1; } \
    && printf '%s\n' "$found" | xargs rm -f \
    && remaining="$(find "$nm" -type f -path '*/@netlify/local-functions-proxy-*/bin/local-functions-proxy')" \
    && [ -z "$remaining" ] || { echo "FATAL: local-functions-proxy binary still present after removal: $remaining" >&2; exit 1; } \
    && echo 'hardened: removed local-functions-proxy executable (deploy-only image)'

COPY entrypoint.sh /usr/local/bin/gocdnext-netlify
RUN chmod +x /usr/local/bin/gocdnext-netlify

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-netlify"]
