# Build context is the repo ROOT — needed so we can COPY docs/ in
# alongside web/. The release workflow sets context: . and
# file: web/Dockerfile to match.
FROM node:20-alpine AS deps
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

FROM node:20-alpine AS build
WORKDIR /src
RUN corepack enable
COPY --from=deps /src/node_modules ./node_modules
COPY web/ ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN pnpm run build

FROM node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000
ENV HOSTNAME=0.0.0.0
RUN addgroup -S -g 1001 nextjs && adduser -S -u 1001 -G nextjs nextjs
COPY --from=build --chown=nextjs:nextjs /src/.next/standalone ./
COPY --from=build --chown=nextjs:nextjs /src/.next/static ./.next/static
# Ship the repo's root-level markdown docs alongside the standalone
# server so the in-app /docs page reads them at runtime. listDocs()
# resolves `<cwd>/../docs`, and cwd is /app — so the markdowns land
# at /docs (sibling to /app), matching the dev layout where cwd is
# /repo/web and parent has /repo/docs.
COPY --chown=nextjs:nextjs docs/*.md /docs/
USER nextjs
EXPOSE 3000
CMD ["node", "server.js"]
