# gocdnext/node — pnpm-first Node.js plugin for gocdnext pipelines.
#
# Contract (see plugins/node/README.md for the full rundown):
#   PLUGIN_COMMAND      (required)  — pnpm subcommand + args, word-split.
#                                     Examples: "install --frozen-lockfile",
#                                     "test --run", "exec tsc --noEmit".
#   PLUGIN_WORKING_DIR  (optional)  — path relative to /workspace; entrypoint
#                                     cd's here before running pnpm. Defaults
#                                     to /workspace (repo root).
#
# The image bundles corepack-enabled node + a shim that resolves pnpm at
# runtime from the repo's `packageManager` field, so the same image serves
# any project without rebuilding per-version.
FROM node:22-alpine

# git is handy for pnpm resolving git-hosted deps; ca-certificates keeps
# the corepack-managed pnpm download (on first run) trusting the
# upstream registry.
RUN apk add --no-cache git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-node
RUN chmod +x /usr/local/bin/gocdnext-node

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-node"]
