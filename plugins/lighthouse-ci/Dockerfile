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
# the multi-arch matrix land. LHCI is pinned by package version
# so a CLI bump becomes a deliberate update commit.
FROM node:20-bookworm-slim

ENV PUPPETEER_SKIP_DOWNLOAD=1 \
    CHROME_PATH=/usr/bin/chromium

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates chromium chromium-sandbox \
        fonts-liberation libnss3 \
    && rm -rf /var/lib/apt/lists/*

ARG LHCI_VERSION=0.14.0
RUN npm install -g "@lhci/cli@${LHCI_VERSION}" \
    && lhci --version

COPY entrypoint.sh /usr/local/bin/gocdnext-lighthouse-ci
RUN chmod +x /usr/local/bin/gocdnext-lighthouse-ci

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-lighthouse-ci"]
