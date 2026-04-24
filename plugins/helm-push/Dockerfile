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
FROM alpine/helm:3.16.2

RUN apk add --no-cache bash curl

COPY entrypoint.sh /usr/local/bin/gocdnext-helm-push
RUN chmod +x /usr/local/bin/gocdnext-helm-push

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-helm-push"]
