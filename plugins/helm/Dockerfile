# gocdnext/helm — helm CLI wrapper. Pairs with gocdnext/kubectl
# for the deploy half of a pipeline: build + push via
# gocdnext/docker, then upgrade the release via this plugin.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND    (required)  helm subcommand + args, word-split.
#                                 Examples:
#                                   "upgrade --install api ./chart -n prod"
#                                   "diff upgrade api ./chart"
#                                   "repo add stable https://..."
#   PLUGIN_KUBECONFIG (optional)  path (relative to /workspace) OR
#                                 inline YAML OR base64-encoded YAML
#                                 (auto-detected). Empty defers to
#                                 the agent's in-cluster service
#                                 account (K8s engine deployments).
#   PLUGIN_NAMESPACE  (optional)  default namespace (`--namespace`).
#   PLUGIN_WORKING_DIR (optional) relative path under /workspace to
#                                 cd into (for local chart paths).
#
# Same kubeconfig discovery as the kubectl plugin so operators
# don't juggle two different conventions between the two halves
# of their deploy stage.
# Base alpine:3.20 (EOL 2026-04-01) -> 3.24.4 (pinned) + apk upgrade. helm bumped
# v3.16.2 -> v3.22.1: v3.16.2 was built on an old Go + grpc (Go-stdlib
# CVE-2025-68121, grpc CVE-2026-33186); v3.22.1 is built on go1.26.9. The download
# is now architecture-aware (TARGETARCH amd64/arm64) and VERIFIED against the
# get.helm.sh sha256 — the old `curl` had no integrity check and hardcoded amd64.
# NOTE: plugins.yml publishes `:v1` linux/amd64 only (fleet-wide); this Dockerfile
# builds correctly for arm64 once that is enabled. Pinned Helm 3 (security fixes to
# 2027-02-10); the Helm 4 migration is tracked in #349.
FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
ARG TARGETARCH=amd64
ENV HELM_VERSION=v3.22.1
# helm-diff is REQUIRED for the advertised `diff upgrade` command — it's an
# external helm plugin, not a helm built-in. Installed from the pinned release
# tarball (prebuilt binary) into HELM_PLUGINS, verified by sha256; no network
# `helm plugin install` at run time.
ENV HELMDIFF_VERSION=v3.15.15
ENV HELM_PLUGINS=/usr/local/share/helm/plugins

RUN apk upgrade --no-cache && apk add --no-cache bash ca-certificates curl git \
    && case "${TARGETARCH}" in \
         amd64) HELM_SHA=896cf63684a42a723d61c980369d6e588330927deb13676ca0d85006dd6da0ea; \
                HELMDIFF_SHA=504e99410611daa70702362e1b10dc74929df8a912cb3f6b5d21d1ee9d0cb7d9 ;; \
         arm64) HELM_SHA=4c29e286b279d170fa048ddfc3e4e83dcd069ca9c52102766ba672d7149e694d; \
                HELMDIFF_SHA=fc046bf3c3f63a435741fe8c37a9d740dfa17ea7f5f0c2a9da2c0222bb4384e6 ;; \
         *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fSL -o /tmp/helm.tgz \
         "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" \
    && echo "${HELM_SHA}  /tmp/helm.tgz" | sha256sum -c - \
    && tar -xz -C /tmp -f /tmp/helm.tgz "linux-${TARGETARCH}/helm" \
    && mv "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm \
    && rm -rf /tmp/helm.tgz "/tmp/linux-${TARGETARCH}" \
    && chmod +x /usr/local/bin/helm \
    && mkdir -p "${HELM_PLUGINS}" \
    && curl -fSL -o /tmp/helm-diff.tgz \
         "https://github.com/databus23/helm-diff/releases/download/${HELMDIFF_VERSION}/helm-diff-linux-${TARGETARCH}.tgz" \
    && echo "${HELMDIFF_SHA}  /tmp/helm-diff.tgz" | sha256sum -c - \
    && tar -xz -C "${HELM_PLUGINS}" -f /tmp/helm-diff.tgz \
    && rm /tmp/helm-diff.tgz && chmod +x "${HELM_PLUGINS}/diff/bin/diff"

COPY entrypoint.sh /usr/local/bin/gocdnext-helm
RUN chmod +x /usr/local/bin/gocdnext-helm

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-helm"]
