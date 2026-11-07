# gocdnext/lighthouse-ci — run Google's Lighthouse CI suite
# against one or more URLs and optionally upload results. The
# canonical "performance budget + accessibility gate" CI step
# for web apps.
#
# LHCI needs Chromium available; the sibling image
# `patrickhulce/lhci-client` ships Chromium + Node + the full
# LHCI CLI, which is what Google's own docs recommend. Use it
# as the base rather than rolling a custom Chromium install.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_URLS         (required if config=auto)  newline- or
#                       comma-separated URLs to audit. Ignored
#                       when a `lighthouserc.json` / `.yml` /
#                       `.js` is present and PLUGIN_CONFIG is
#                       empty (auto-discover).
#   PLUGIN_CONFIG       (optional)  explicit config file path
#                       (workspace-relative). Overrides the
#                       URLS input. LHCI reads this verbatim,
#                       so full settings surface is available.
#   PLUGIN_NUMBER_OF_RUNS (optional)  samples per URL. Default 3
#                       — matches LHCI's default; odd numbers
#                       trade off time for median stability.
#   PLUGIN_UPLOAD_TARGET (optional)  "temporary-public-storage" |
#                       "filesystem" | "lhci". Default
#                       "temporary-public-storage" — dumps to
#                       Google's 7-day bucket with a link per
#                       run, zero config.
#   PLUGIN_UPLOAD_SERVER_BASE_URL (optional)  target when
#                       upload-target=lhci; the URL of an LHCI
#                       server you run internally.
#   PLUGIN_UPLOAD_TOKEN (optional)  build token when
#                       upload-target=lhci. Pipe via secrets:.
#   PLUGIN_ASSERTIONS   (optional)  "on" toggles `lhci assert`
#                       after collect — uses the assertion
#                       config from lighthouserc. Default "on"
#                       because the whole point of a CI step is
#                       to fail builds on budget misses.
# patrickhulce/lhci-client (the upstream "Chromium + LHCI" image)
# stopped publishing the 0.14.x tag, so we build the runtime
# ourselves on top of node + Debian Bookworm's chromium package.
# Both packages ship for amd64 AND arm64, which is what makes
# the multi-arch matrix land.
#
# Base pinned by version+digest. This image bundles Chromium, which processes
# potentially hostile web content, so a STALE build is a real exposure. The
# previously published :v1 carried ~280 CRITICAL because the apt layer had been
# cached at Chromium 149 and never refreshed. Two measures: (1) the base is
# digest-pinned so the apt layer refreshes exactly when the digest is bumped,
# and (2) `apt-get upgrade` applies Debian security backports at build time
# (so OS packages, incl. Chromium, are current — Chromium 156+ now). The weekly
# Trivy gate catches drift and forces the next bump.
#
# NOTE: `apt-get upgrade` pulls from Debian's MUTABLE security repos, so the
# OS-package set is security-patched at build time, not byte-identical
# build-to-build; the FROM digest pins only the base layer.
FROM node:22-bookworm-slim@sha256:5ec23b7c622b00547753bc6ffd78085dd868a68aeb41c8e0a1418f033e13d96d

ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    CHROME_PATH=/usr/bin/chromium

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        bash ca-certificates chromium chromium-sandbox \
        fonts-liberation libnss3 \
    && rm -rf /var/lib/apt/lists/*

# LHCI pinned by version. Installed into a project dir (not `-g`) so an
# `overrides` can force the patched node-tar (>= 7.5.19, CVE-2026-59873) that
# @lhci/cli otherwise pulls transitively at 6.2.1 — the smoke proves LHCI still
# collects/asserts on the overridden tar. `lhci` is symlinked onto PATH.
ARG LHCI_VERSION=0.16.0
WORKDIR /opt/lhci
RUN printf '{"name":"gocdnext-lhci","private":true,"dependencies":{"@lhci/cli":"%s"},"overrides":{"tar":">=7.5.19"}}' \
        "${LHCI_VERSION}" > package.json \
    && npm install --omit=dev --no-audit --no-fund \
    && ln -sf /opt/lhci/node_modules/.bin/lhci /usr/local/bin/lhci \
    && lhci --version

COPY entrypoint.sh /usr/local/bin/gocdnext-lighthouse-ci
RUN chmod +x /usr/local/bin/gocdnext-lighthouse-ci

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-lighthouse-ci"]
