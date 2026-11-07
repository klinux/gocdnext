# gocdnext/vercel — deploy to Vercel.
#
# Contract:
#   VERCEL_TOKEN       (required)  via `secrets:`.
#   VERCEL_ORG_ID / VERCEL_PROJECT_ID (required via `secrets:` or
#                                  variables — the CLI's headless
#                                  project linkage).
#   PLUGIN_PROD        (optional)  production vs preview.
#   PLUGIN_PREBUILT    (optional)  deploy .vercel/output produced
#                                  by `vercel build` in a prior job.
#
# Base pinned by digest (node:22-alpine = supported Alpine 3.24) + `apk upgrade`
# for OS currency. Opts into `no_cache` (plugin.yaml) so the publish build can't
# freeze a stale apk layer. NOTE: `apk upgrade` pulls Alpine's MUTABLE repos — the
# FROM digest pins the base layer, not the upgraded set; the weekly Trivy gate
# catches drift.
FROM node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202

# Vercel CLI bumped v37 -> v65.12.1: v37 bundled an esbuild built on Go 1.18.3 (4
# Go-stdlib CRITICAL incl. CVE-2025-68121) and vulnerable node-tar (CVE-2026-59873)
# — v65 bundles a modern esbuild, and an `overrides` pins node-tar to the patched
# 7.5.x (a patch-level bump within tar 7.x; one transitive copy lagged at 7.5.11).
# Installed from a COMMITTED package.json + package-lock.json via `npm ci`, so the
# WHOLE transitive tree is pinned + integrity-verified and reproducible across
# no_cache rebuilds. Regenerate the lock with `npm install` when bumping vercel.
WORKDIR /opt/vercel
COPY package.json package-lock.json ./
RUN apk upgrade --no-cache \
    && npm ci --omit=dev --no-audit --no-fund \
    && ln -sf /opt/vercel/node_modules/.bin/vercel /usr/local/bin/vercel \
    && npm cache clean --force \
    && vercel --version

COPY entrypoint.sh /usr/local/bin/gocdnext-vercel
RUN chmod +x /usr/local/bin/gocdnext-vercel

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-vercel"]
