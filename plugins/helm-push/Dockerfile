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
# Base migrated alpine/helm:3.22.1 -> 4.3.1 (pinned by digest). Resolves #349:
# a PROACTIVE migration ahead of the Helm 3 security end-of-life, which the Helm
# project has set for 2027-02-10 (https://helm.sh/blog/helm-v3-end-of-life/,
# extended from Nov 2026 — on-demand security patches continue until then, after
# which 3.x gets no releases of any kind). Moving now, while 3.x is still patched,
# avoids a scramble when a late CVE in the shipped `helm` binary would have no
# patched 3.x base to move to. Helm 4.3.1 is built on a current Go and is the
# actively-maintained line.
#
# The plugin's CLI surface is UNCHANGED across the v3->v4 major: the only helm
# commands this plugin runs are `helm package` (flags --destination, --version,
# --app-version), `helm push` (OCI), and `helm registry login --username
# --password-stdin <host>` — all present and behaviour-compatible in 4.3.1
# (verified against the real binary + the smoke: package of a minimal
# apiVersion:v2 chart, version/app-version overrides, and push/login reaching the
# registry boundary rather than a parse error). The chartmuseum/nexus backends
# use curl, not helm, so they are unaffected by the helm major.
#
# `apk upgrade` refreshes the OS set; the FROM digest pins the base layer, not the
# upgraded set, so the weekly Trivy gate catches drift.
FROM alpine/helm:4.3.1@sha256:e3f8d5c462d75470fbb1fce79ad5c20063657096d1f98c0fcea869e2600e2e5e

RUN apk upgrade --no-cache && apk add --no-cache bash curl

COPY entrypoint.sh /usr/local/bin/gocdnext-helm-push
RUN chmod +x /usr/local/bin/gocdnext-helm-push

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-helm-push"]
