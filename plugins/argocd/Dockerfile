# gocdnext/argocd — argocd CLI wrapper for teams running Argo CD
# alongside gocdnext. Typical CI/CD split: gocdnext drives
# the build + push pipeline, then this plugin nudges Argo CD
# to sync the new image. Argo's reconciler is the source of
# truth for desired state; gocdnext just hands it the trigger.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_SERVER         (required)  Argo CD API server URL
#                                     (e.g. https://argocd.corp).
#   PLUGIN_AUTH_TOKEN     (required)  Argo CD API token (created
#                                     via `argocd account
#                                     generate-token`). Pipe
#                                     through secrets:.
#   PLUGIN_COMMAND        (required)  argocd subcommand + args,
#                                     word-split. Examples:
#                                       "app sync my-app"
#                                       "app wait my-app --health"
#                                       "app rollback my-app 3"
#   PLUGIN_INSECURE       (optional)  "true" → --insecure (skip TLS
#                                     verification). Off by default.
#                                     Mutually exclusive with CA_CERT.
#   PLUGIN_CA_CERT        (optional)  PEM of the CA that signed the Argo
#                                     CD server cert (internal/corp CA).
#                                     Written to a file + passed as
#                                     --server-crt so TLS is VERIFIED,
#                                     not skipped. Mutually exclusive
#                                     with INSECURE.
#   PLUGIN_GRPC_WEB       (optional)  "true" → --grpc-web (use
#                                     when Argo is behind an L7
#                                     reverse proxy that doesn't
#                                     speak HTTP/2 to backends).
#   PLUGIN_PLUGIN_ENV     (optional)  a single config-management
#                                     plugin env, forwarded as one
#                                     `--plugin-env NAME=value` arg
#                                     so a multi-token Helm CMP value
#                                     ("HELM_ARGS=--set x -f y")
#                                     survives intact. Pairs with
#                                     `command: app set <app>`.
#
# The CLI auths via the token in env (ARGOCD_AUTH_TOKEN) — no
# config file persistence needed, every run is fresh.
# Base: a SUPPORTED Alpine (3.24), pinned by patch+digest, with `apk upgrade` for
# OS security currency. 3.20 went EOL 2026-04-01 — an unsupported base stops
# getting security updates (and Trivy's own DB coverage degrades: "OS version is
# no longer supported"), which `apk upgrade` can't fix. Opts into `no_cache`
# (plugin.yaml) so the publish build can't freeze a stale apk layer.
# NOTE: `apk upgrade` pulls Alpine's MUTABLE repos — the FROM digest pins the base
# layer, not the upgraded set; the weekly Trivy gate catches drift.
FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4

# argocd CLI bumped v2.13.1 -> v3.6.1: v2.13.1 carried 6 CRITICAL in its Go binary
# (argo-cd CVE-2025-47933/CVE-2025-55190/CVE-2026-77459, go-git CVE-2025-21613,
# grpc CVE-2026-33186, Go-stdlib CVE-2025-68121). v3.6.1 is built with Go 1.27.3
# and bundles the patched deps -> Trivy on the binary: 0 CRITICAL. The download is
# now CHECKSUM-VERIFIED against the release's published sha256 (it previously
# curled the binary with no verification). Bump = new version + sha256 here.
ENV ARGOCD_VERSION=v3.6.1
ARG ARGOCD_SHA256=46d2fc941071f0a0c36326a67a431d34b1479e1a24737f7b367abc93968beadc

RUN apk upgrade --no-cache \
    && apk add --no-cache bash ca-certificates curl \
    && curl -fsSL -o /usr/local/bin/argocd \
       "https://github.com/argoproj/argo-cd/releases/download/${ARGOCD_VERSION}/argocd-linux-amd64" \
    && echo "${ARGOCD_SHA256}  /usr/local/bin/argocd" | sha256sum -c - \
    && chmod +x /usr/local/bin/argocd \
    && argocd version --client

COPY entrypoint.sh /usr/local/bin/gocdnext-argocd
RUN chmod +x /usr/local/bin/gocdnext-argocd

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-argocd"]
