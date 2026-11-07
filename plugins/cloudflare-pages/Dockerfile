# gocdnext/cloudflare-pages — deploy a built directory to
# Cloudflare Pages via wrangler.
#
# Contract:
#   CLOUDFLARE_API_TOKEN  (required)  via `secrets:` — wrangler
#                                     reads it natively from env.
#   CLOUDFLARE_ACCOUNT_ID (required)  via `secrets:` or variables.
#   PLUGIN_DIR            (required)  the built site directory.
#   PLUGIN_PROJECT_NAME   (required)  Pages project.
#   PLUGIN_BRANCH         (optional)  deployment branch (Pages maps
#                                     non-production branches to
#                                     preview URLs).
#
# Base pinned by digest + `apk upgrade` (clears OS drift; the weekly
# Trivy gate catches new OS CVEs). wrangler bumped 3 -> 4.158.0: v3
# bundled an esbuild built on Go 1.20.4 (Go-stdlib CVE-2024-24790 +
# CVE-2025-68121); v4 ships esbuild 0.28.x on a patched Go. The plugin
# surface (`wrangler pages deploy <dir> --project-name --branch`) is
# unchanged across the v3->v4 major. Deps are installed with `npm ci`
# from a committed package-lock.json so the npm dependency tree is
# locked/reproducible and the gate scans a frozen tree (no build-time
# drift of transitive deps like esbuild/tar). NOTE: `apk upgrade`
# consults Alpine's mutable repos, so the OS layer is not bit-for-bit
# reproducible — the FROM digest pins the base, the weekly gate catches
# OS drift.
FROM node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202

RUN apk upgrade --no-cache

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
ENV PATH="/app/node_modules/.bin:${PATH}"

# CI plugin: never phone home. Disable wrangler's telemetry so no deviceId /
# metrics leave the job (and so the smoke stays fully offline-isolated).
ENV WRANGLER_SEND_METRICS=false

COPY entrypoint.sh /usr/local/bin/gocdnext-cloudflare-pages
RUN chmod +x /usr/local/bin/gocdnext-cloudflare-pages

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-cloudflare-pages"]
