# gocdnext/nuclei — DAST baseline scanner. Wraps ProjectDiscovery's Nuclei to
# scan a *running* target (an ephemeral app booted via `services:`, or an
# ephemeral env) and emits SARIF that feeds the Security dashboard (#71).
#
# v1 is an unauthenticated baseline + API-spec-driven scanner — see entrypoint.sh
# for the security posture (no auth/headers, OAST off, redirects off, HTTP
# templates only, templates baked + never updated at runtime).
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_TARGET         (required)  base URL to scan, e.g. http://app:8080.
#   PLUGIN_SEVERITY       (optional)  scan scope (default critical,high,medium).
#   PLUGIN_FAIL_ON        (optional)  fail threshold ⊆ severity (default critical,high).
#   PLUGIN_EXIT_CODE      (optional)  exit when fail-on matches (default 1; 0 = advisory).
#   PLUGIN_SPEC           (optional)  OpenAPI/Swagger file → API-driven scan; its
#                                     base URL is rewritten to PLUGIN_TARGET.
#   PLUGIN_SPEC_FORMAT    (optional)  openapi | swagger (auto-detected; default openapi).
#   PLUGIN_TAGS           (optional)  -tags filter.
#   PLUGIN_TEMPLATES      (optional)  override the baked template dir.
#   PLUGIN_READY_TIMEOUT  (optional)  preflight wait seconds (default 60).
#   PLUGIN_HEALTH_PATH    (optional)  preflight path (requires 2xx/3xx).
#   PLUGIN_RATE_LIMIT     (optional)  requests/sec (default 50).
#   PLUGIN_CONCURRENCY    (optional)  template concurrency.
#   PLUGIN_TIMEOUT        (optional)  per-request timeout seconds (default 10).
#   PLUGIN_INTERACTSH     (optional)  "true" to enable OAST (default off).
#   PLUGIN_INTERACTSH_SERVER (optional) custom OAST server (needs interactsh:true).
#   PLUGIN_OUTPUT         (optional)  SARIF path (default nuclei.sarif).
#
# nuclei bumped v3.3.2 -> v3.11.1 (pinned by digest): v3.3.2 was built on Go
# 1.21.13 and bundled vulnerable deps (stdlib CVE-2025-68121, go-git
# CVE-2025-21613, kin-openapi GHSA-r277-6w6q-xmqw); v3.11.1 is built on go1.26.5
# with those deps patched. Runtime base alpine:3.20 (EOL 2026-04-01) -> 3.24.4
# (pinned) + apk upgrade. yq bumped v4.44.3 -> v4.54.2 (old yq was built on an
# old Go: stdlib CVE-2025-68121) and now verified by sha256. Trivy on the built
# image (DB 2026-11-07): 0 CRITICAL (goal 4 -> 0). 27 HIGH remain in the nuclei
# binary (stdlib/kin-openapi/grpc/go-git/x-net/…) — v3.11.1 is the latest upstream
# release, so they are upstream-bound; the weekly Trivy gate tracks them.
ARG NUCLEI_VERSION=v3.11.1

FROM projectdiscovery/nuclei:v3.11.1@sha256:582d5546902e67052097cb2d07296c642d50a1afc5e44623cb038845df9a32eb AS nuclei

FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
ARG NUCLEI_VERSION=v3.11.1
ARG NUCLEI_TEMPLATES_REF=v10.4.5
# Immutable pin: the exact commit the v10.4.5 tag points at. A Git tag is movable,
# and with no_cache:true every release re-resolves it — verify the clone landed on
# this commit and fail loud if the tag was re-pointed.
ARG NUCLEI_TEMPLATES_COMMIT=e5a0fba09e7e9a2838f9aa0c0bd2cb30ce18c202
ARG YQ_VERSION=v4.54.2
ARG TARGETARCH=amd64

# bash (entrypoint), curl (preflight), jq (fail-on gate), yq (spec rewrite),
# git (pinned templates clone). apk upgrade refreshes the OS set (the FROM digest
# pins the base layer, not the upgraded set; the weekly Trivy gate catches drift).
# yq is downloaded from a GitHub release and verified against a pinned sha256
# (matches yq's published checksums) — a floating download is a supply-chain hole.
RUN apk upgrade --no-cache \
 && apk add --no-cache bash curl jq git ca-certificates \
 && case "${TARGETARCH}" in \
      amd64) YQ_SHA=44c7759059b4910827a0059a2757b22001e59360cdecdb2ef8dedc4b6c7e9f32 ;; \
      arm64) YQ_SHA=bd44211dcd29a5f0eb701d4b7eb5ccd0cfff3bd3966cb4f7f9bd5b13701d997e ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && wget -qO /usr/local/bin/yq \
      "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${TARGETARCH}" \
 && echo "${YQ_SHA}  /usr/local/bin/yq" | sha256sum -c - \
 && chmod +x /usr/local/bin/yq

COPY --from=nuclei /usr/local/bin/nuclei /usr/local/bin/nuclei

# Templates: PINNED + baked at BUILD time, runtime NEVER updates
# (-disable-update-check) → deterministic (same image = same templates). Clone
# the EXACT tag rather than `nuclei -update-templates` (which floats to whatever
# is latest at build time), drop .git to keep the layer lean, and fail loud if
# they didn't land — the "builds fine, runtime empty" trap.
ENV NUCLEI_TEMPLATES_DIR=/opt/nuclei-templates
RUN git clone --depth 1 --branch "${NUCLEI_TEMPLATES_REF}" \
      https://github.com/projectdiscovery/nuclei-templates /opt/nuclei-templates \
 && got="$(git -C /opt/nuclei-templates rev-parse HEAD)" \
 && [ "${got}" = "${NUCLEI_TEMPLATES_COMMIT}" ] \
      || { echo "templates tag ${NUCLEI_TEMPLATES_REF} resolved to ${got}, expected ${NUCLEI_TEMPLATES_COMMIT} (tag re-pointed?)" >&2; exit 1; } \
 && rm -rf /opt/nuclei-templates/.git \
 && test -n "$(ls -A /opt/nuclei-templates 2>/dev/null)" \
      || { echo "templates not baked into /opt/nuclei-templates" >&2; exit 1; }

LABEL org.opencontainers.image.title="gocdnext-plugin-nuclei" \
      org.opencontainers.image.description="DAST baseline scanner (Nuclei) → SARIF" \
      gocdnext.nuclei.version="${NUCLEI_VERSION}" \
      gocdnext.nuclei.templates="${NUCLEI_TEMPLATES_REF}" \
      gocdnext.nuclei.templates_commit="${NUCLEI_TEMPLATES_COMMIT}"

COPY entrypoint.sh /usr/local/bin/gocdnext-nuclei
RUN chmod +x /usr/local/bin/gocdnext-nuclei

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-nuclei"]
