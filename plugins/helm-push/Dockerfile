# gocdnext/helm-push — package + push a Helm chart to an OCI
# registry (ghcr.io, Docker Hub, Harbor, ECR, Artifactory OCI,
# GCR, …) OR to a chart-museum / Nexus chart repository.
# Different backends need slightly different commands; the plugin
# auto-detects via `backend: oci | chartmuseum | nexus`, defaulting
# to "oci" (the 2025+ convention per Helm 3.8+ GA).
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_CHART_DIR    (optional)  workspace-relative path to the
#                                   chart directory. Default ".".
#                                   The chart is packaged to a
#                                   tgz under /tmp before push.
#   PLUGIN_VERSION      (optional)  --version override for
#                                   `helm package`. Uses the
#                                   Chart.yaml value when unset.
#   PLUGIN_APP_VERSION  (optional)  --app-version override for
#                                   `helm package`.
#   PLUGIN_BACKEND      (optional)  "oci" (default) | "chartmuseum"
#                                   | "nexus".
#
#   # OCI backend:
#   PLUGIN_OCI_REPO     (required for oci)  target repo, e.g.
#                       "oci://ghcr.io/acme/charts".
#   PLUGIN_USERNAME     (optional)  registry user. Pipe via secrets.
#   PLUGIN_PASSWORD     (optional)  registry token/password.
#
#   # chartmuseum / nexus backend:
#   PLUGIN_REPO_URL     (required for chartmuseum/nexus)  base URL
#                       of the chart repo. For nexus this is the
#                       full hosted-helm-repo URL (e.g.
#                       https://nexus.corp/repository/helm/).
# Base bumped alpine/helm:3.16.2 -> 3.22.1 (pinned by digest). 3.16.2 shipped the
# `helm` binary built on an old Go that bundled vulnerable stdlib + grpc
# (Go-stdlib CVE-2025-68121, grpc CVE-2026-33186); 3.22.1 is built on Go 1.26.9 and
# rides Alpine 3.24. The plugin's surface is unchanged — it stays on the Helm 3
# line (NOT 4.x, which is a breaking major): `helm package`, `helm push`,
# `helm registry login` and the flags it passes (--destination, --version,
# --app-version, --username, --password-stdin) all exist in 3.22.1 (proven by the
# smoke). `apk upgrade` refreshes the OS set (clears brotli-libs CVE-2026-111497
# CRITICAL); the FROM digest pins the base layer, not the upgraded set, so the
# weekly Trivy gate catches drift.
FROM alpine/helm:3.22.1@sha256:8191701ad1f82ee3695c87211fcbfe6d7b1bb7df92ac1c8de9ccdb0995a6a2c1

RUN apk upgrade --no-cache && apk add --no-cache bash curl

COPY entrypoint.sh /usr/local/bin/gocdnext-helm-push
RUN chmod +x /usr/local/bin/gocdnext-helm-push

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-helm-push"]
