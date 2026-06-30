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
ARG NUCLEI_VERSION=v3.3.2

FROM projectdiscovery/nuclei:${NUCLEI_VERSION} AS nuclei

FROM alpine:3.20
ARG NUCLEI_VERSION=v3.3.2
ARG NUCLEI_TEMPLATES_REF=v10.4.5
ARG YQ_VERSION=v4.44.3
ARG TARGETARCH=amd64

# bash (entrypoint), curl (preflight), jq (fail-on gate), yq (spec rewrite),
# git (pinned templates clone).
RUN apk add --no-cache bash curl jq git ca-certificates \
 && wget -qO /usr/local/bin/yq \
      "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${TARGETARCH}" \
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
 && rm -rf /opt/nuclei-templates/.git \
 && test -n "$(ls -A /opt/nuclei-templates 2>/dev/null)" \
      || { echo "templates not baked into /opt/nuclei-templates" >&2; exit 1; }

LABEL org.opencontainers.image.title="gocdnext-plugin-nuclei" \
      org.opencontainers.image.description="DAST baseline scanner (Nuclei) → SARIF" \
      gocdnext.nuclei.version="${NUCLEI_VERSION}" \
      gocdnext.nuclei.templates="${NUCLEI_TEMPLATES_REF}"

COPY entrypoint.sh /usr/local/bin/gocdnext-nuclei
RUN chmod +x /usr/local/bin/gocdnext-nuclei

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-nuclei"]
