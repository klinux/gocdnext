# Build context is the repo ROOT — needed so we can COPY docs/ in
# alongside web/. The release workflow sets context: . and
# file: web/Dockerfile to match.
#
# Base: node 24-alpine, pinned by digest (#391). The three stages previously
# ran on node:20-alpine — node 20 reached end-of-life on 2026-04-30 (no
# upstream security/bug fixes), and the `runner` stage is the PRODUCTION
# runtime serving the app. node 24 is Maintenance LTS (EOL 2028-04-30 — node 26
# is the current Active LTS, but 24 gives the longest still-security-supported
# runway while keeping bundled corepack: 0.36.0, which the pnpm@10
# `packageManager` pin needs; node 26 unbundled corepack). node 24 satisfies
# Next 16's `engines.node >= 20.9.0`. The move clears 3 base CRITICAL + most HIGH
# that node:20-alpine carried (Trivy, --ignore-unfixed). digest pins the layer;
# a bump refreshes it (same convention as the plugin bases). docker_smoke.sh
# builds this file and boots the runner stage (GET /docs → 200) so the
# production runtime is regression-covered, not just `pnpm build`.
#
# web/Dockerfile.dockerignore keeps the host's web/node_modules + web/.next OUT
# of the build context so `COPY web/ ./` below can't overwrite the clean,
# in-image install from the deps stage with host-built (glibc) artifacts.
FROM node:24-alpine@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8 AS deps
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile

FROM node:24-alpine@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8 AS build
WORKDIR /src
RUN corepack enable
COPY --from=deps /src/node_modules ./node_modules
COPY web/ ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN pnpm run build

FROM node:24-alpine@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8 AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000
ENV HOSTNAME=0.0.0.0
RUN addgroup -S -g 1001 nextjs && adduser -S -u 1001 -G nextjs nextjs
COPY --from=build --chown=nextjs:nextjs /src/.next/standalone ./
COPY --from=build --chown=nextjs:nextjs /src/.next/static ./.next/static
# Ship the docs site's markdown (same content the public Starlight site uses)
# alongside the standalone server so the in-app /docs page reads it at runtime.
# listDocs() resolves `<cwd>/../docs/src/content/docs`, and cwd is /app — so the
# content lands at /docs/src/content/docs (sibling to /app), matching dev where
# cwd is /repo/web and parent has /repo/docs/src/content/docs.
COPY --chown=nextjs:nextjs docs/src/content/docs /docs/src/content/docs
USER nextjs
EXPOSE 3000
CMD ["node", "server.js"]
