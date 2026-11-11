# gocdnext/node v3 — Node.js install + run runner with MULTI-VERSION node
# selection + bun. See plugin.yaml for the input contract; entrypoint.sh
# implements it; select-node.sh does the version switch.
#
# v4 vs v3 (#373 — drop EOL node 20):
#   - Bakes node 22 + 24 + 26 in ONE image, selected at runtime by
#     select-node.sh from the project's engines.node / .nvmrc /
#     .node-version (or the node-version: input) — a pure PATH switch, no
#     runtime download. Same "one image, select the version" convention as
#     jdk-base (Temurin 11/17/21/25) and dotnet (SDK 8/10).
#   - BREAKING vs v3: node 20 is dropped. It reached EOL on 2026-04-30 and
#     no longer receives upstream security/bug fixes, so v4 does not bake
#     it. A pipeline whose version resolves to 20 (engines.node 20.x,
#     node-version: 20, .nvmrc 20, a <22 range) now FAILS LOUD (exit 2)
#     with the baked set + a "pin a supported major or use a custom image"
#     message — never a silent run on a different node. Migrate to 22|24|26.
#     v3 is FROZEN at its last published digest (still baking 20/22/24) for
#     pipelines pinned to @v3; it is not rebuilt. See README migration note.
#   - Baked majors as of 2026-11: 22 Maintenance LTS (EOL 2027-04-30),
#     24 Maintenance LTS (EOL 2028-04-30), 26 Active LTS (EOL 2029-04-30).
#     Default stays node 22 (hint-less projects keep v3 behaviour).
#   - Adds bun as a first-class manager (corepack can't manage bun, so
#     the binary is baked from oven/bun).
#   - Base is debian-slim (glibc): matches the node:24 base real projects
#     build on, and avoids musl prebuilt-native-module surprises.
#     build-essential + python3 make node-gyp native builds work out of
#     the box.

# Per-major node builders — the official images install node + npm +
# corepack under /usr/local; we relocate each to a versioned path.
# Pinned by digest (trixie-slim; see the final stage for the OS-CVE
# rationale).
#
# npm/node-tar posture (CVE-2026-59873: node-tar < 7.5.19):
#   - node 22 ships npm 10.x whose BUNDLED node-tar (7.5.11) is < 7.5.19,
#     so it is bumped to npm 11.19.1 (bundles tar 7.5.22). Pinning the same
#     npm keeps the npm<->tar API aligned (npm 11 expects tar 7); surgically
#     swapping only tar would cross tar's 6->7 break under npm 10.
#   - node 24 is already clean (npm 11.19.1 / tar 7.5.22) → untouched.
#   - node 26 ships npm 11.20.0 / tar 7.5.22 (already >= 7.5.19) → no bump.
#
# corepack on node 26: node STOPPED distributing corepack in the runtime as
# of node 25 (https://github.com/nodejs/node/blob/main/doc/contributing/distribution.md),
# so node:26 has NO corepack. The plugin drives yarn v3+/pnpm through
# corepack, so we install it standalone, PINNED to 0.36.0 — the exact
# version node 22/24 still bundle — keeping corepack consistent across the
# baked set. It installs under /usr/local and rides the relocation like the
# bundled one; the entrypoint runs `corepack enable` at RUNTIME (so no yarn
# shim is baked here — the relocation step below still strips the official
# image's dangling yarn1 shim).
#
# (Only /usr/local is copied out of these stages, so their OS packages never
# reach the scanned final image; they move to trixie purely to keep node's
# glibc baseline consistent with the trixie runtime.)
FROM node:22-trixie-slim@sha256:be1e2a3dc2c4fd68cdfadad2a696a280e0d2749098bee509230174d72fb768c6 AS node22
RUN npm install -g npm@11.19.1
FROM node:24-trixie-slim@sha256:0820eaa85e07d0490446dd01d14707e17e6108a36988cf374899cd057eb3812d AS node24
FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072 AS node26
RUN npm install -g corepack@0.36.0

# bun binary source (glibc slim variant to match the debian final stage).
# Pinned to an EXACT version for reproducibility — bun is not
# corepack-version-managed, so whatever is baked here is what every job
# runs; a floating `1-slim` would silently change the bun across rebuilds.
# The entrypoint warns when a repo's `packageManager: "bun@x"` differs
# from this baked version; an exact-version need is the custom-image path.
FROM oven/bun:1.4.0-slim@sha256:e0ee68d16ccb9927bf02aa7dd8fd4bf3369ee6d46da04faa72b05ce8bfd135f6 AS bun

# Debian trixie (13): bookworm's libsqlite3-0 (CVE-2025-7458),
# linux-libc-dev (CVE-2026-99083) and zlib1g (CVE-2023-45853) have no fix in
# bookworm — all three land in trixie. Pinned by digest.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

# Runtime toolchain every node CI step may touch:
#   - ca-certificates: TLS to npm/registries + corepack binary fetch.
#   - git: managers resolving git-hosted deps + the safe.directory fix.
#   - bash: entrypoint uses `bash -lc` array idioms.
#   - build-essential + python3: node-gyp native module compiles
#     (g++/make/python3) — the axis a fixed language image otherwise
#     can't cover. openssl ships in the base.
# apt-get upgrade refreshes OS packages fixed since the base digest was
# cut (the digest pins the layer; the weekly Trivy gate catches drift —
# hence no_cache:true). The sqlite3/linux-libc-dev/zlib1g CRITICALs that
# had no bookworm fix are resolved by the trixie base above.
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
      ca-certificates git bash build-essential python3 \
    && rm -rf /var/lib/apt/lists/*

# Relocate each node install to /opt/node/node-<major>. The official
# node images keep node + npm + corepack (+ a yarn1 shim) under
# /usr/local with relative symlinks, so copying the whole tree yields a
# self-contained toolchain that select-node.sh addresses by PATH.
COPY --from=node22 /usr/local /opt/node/node-22
COPY --from=node24 /usr/local /opt/node/node-24
COPY --from=node26 /usr/local /opt/node/node-26

# The official node images ship a bundled yarn 1.x (symlinked to
# /opt/yarn-v1.22.22, OUTSIDE /usr/local so it's NOT copied) plus a
# `nodejs` alias pointing at an ABSOLUTE /usr/local path — both DANGLE
# once node is relocated to /opt/node, and the dangling yarn trips
# `corepack enable`'s realpath scan. The plugin rejects yarn v1 and
# drives yarn v3+ through corepack (which recreates its own pnpm/yarn
# shims at runtime), so drop the stale yarn links. The `nodejs` alias is
# recreated as a RELATIVE symlink so scripts calling `nodejs` still work
# (the official images provide it). npm/npx/corepack already use relative
# symlinks and survive the relocation untouched.
RUN for d in /opt/node/node-*; do \
      rm -f "$d/bin/yarn" "$d/bin/yarnpkg" "$d/bin/nodejs"; \
      ln -s node "$d/bin/nodejs"; \
    done

# bun: single static binary on PATH for every job regardless of the
# selected node major.
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun

# Default to node 22 (Maintenance LTS; kept as the default for v3 parity —
# hint-less projects keep the same node they ran on v3).
# A job that somehow runs without sourcing select-node.sh still boots a
# working node — fail-closed to working, like jdk-base's default JAVA_HOME.
ENV PATH=/opt/node/node-22/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

COPY select-node.sh /usr/local/bin/select-node.sh
COPY entrypoint.sh /usr/local/bin/gocdnext-node
RUN chmod +x /usr/local/bin/select-node.sh /usr/local/bin/gocdnext-node

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-node"]
