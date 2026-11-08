# gocdnext/kubectl — apply manifests / rollout / generic kubectl.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND    (required)  kubectl subcommand + args, word-split.
#                                 Examples: "apply -f k8s/",
#                                 "rollout status deploy/api -n prod",
#                                 "get pods -o wide".
#   PLUGIN_KUBECONFIG (optional)  path (relative to /workspace) OR base64
#                                 inline config. If set, gets written to
#                                 /tmp/kubeconfig and KUBECONFIG points
#                                 at it. Empty = rely on the cluster's
#                                 in-pod service account (when the agent
#                                 runs in-cluster).
#   PLUGIN_NAMESPACE  (optional)  default namespace via `--namespace`.
#
# Operators who need auth via cloud-provider plugins
# (aws-iam-authenticator / gke-gcloud-auth-plugin) should start
# with this image as a base and layer those tools in — the MVP
# plugin stays minimal to keep the image small.
# Base alpine:3.20 (EOL 2026-04-01) -> 3.24.4 (pinned) + apk upgrade. kubectl
# bumped v1.31.1 -> v1.37.2 (latest stable): v1.31.1 was built on an old Go
# (Go-stdlib CVE-2025-68121); v1.37.2 is on a patched Go. The download is now
# architecture-aware (TARGETARCH, amd64/arm64) and VERIFIED against the official
# dl.k8s.io sha256 — the old `curl` had no integrity check (a supply-chain hole)
# and hardcoded amd64. NOTE: plugins.yml currently publishes `:v1` linux/amd64
# only (fleet-wide); this Dockerfile builds correctly for arm64 once that is
# enabled. NOTE on skew: kubectl
# is supported within ±1 minor of the API server; core verbs work across a wider
# range, and operators needing an exact match derive this image and re-pin kubectl.
FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
ARG TARGETARCH=amd64

RUN apk upgrade --no-cache && apk add --no-cache bash ca-certificates curl \
    && case "${TARGETARCH}" in \
         amd64) KUBECTL_SHA=c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4 ;; \
         arm64) KUBECTL_SHA=fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509 ;; \
         *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fSL -o /usr/local/bin/kubectl \
         "https://dl.k8s.io/release/v1.37.2/bin/linux/${TARGETARCH}/kubectl" \
    && echo "${KUBECTL_SHA}  /usr/local/bin/kubectl" | sha256sum -c - \
    && chmod +x /usr/local/bin/kubectl

COPY entrypoint.sh /usr/local/bin/gocdnext-kubectl
RUN chmod +x /usr/local/bin/gocdnext-kubectl

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-kubectl"]
