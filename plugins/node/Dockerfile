# gocdnext/node v2 — Node.js install + run runner. See plugin.yaml
# for the full input contract; entrypoint.sh implements it.
#
# v2 vs v1: now supports multi-manager (pnpm/npm/yarn v3+/none),
# install knob (skippable for artifact-restore jobs), shell-eval
# command (bash -lc — `&&`, pipes, redirects all work).
FROM node:22-alpine

# bash is required at runtime (entrypoint uses `bash -lc` to shell-eval
# the operator's command). alpine's default sh (ash) doesn't handle the
# array idioms in entrypoint.sh; node:22-alpine doesn't include bash
# by default. git is handy for managers resolving git-hosted deps.
# ca-certificates keeps the corepack-managed binary download (on first
# run) trusting upstream registries.
RUN apk add --no-cache bash git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-node
RUN chmod +x /usr/local/bin/gocdnext-node

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-node"]
