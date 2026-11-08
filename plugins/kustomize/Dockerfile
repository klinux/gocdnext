# gocdnext/kustomize — kustomize render + apply wrapper. Pairs with
# gocdnext/kubectl + gocdnext/helm for the deploy half of a pipeline:
# build manifests from a kustomization tree and apply / build / diff /
# validate them — with optional image-tag pinning, ${VAR} substitution,
# and a rollout wait. See plugin.yaml for the full input contract +
# examples; the key PLUGIN_* env (mapped from `with:`):
#   PLUGIN_PATH        (required)  kustomization root under /workspace.
#   PLUGIN_ACTION      (optional)  apply | build | diff | validate (apply).
#   PLUGIN_IMAGES      (optional)  newline/comma list of `kustomize edit
#                                  set image` overrides (name=img:tag).
#   PLUGIN_ENVSUBST    (optional)  "true" (all set vars) or a comma/space
#                                  list of var names to substitute in the
#                                  rendered manifests (inject secrets).
#   PLUGIN_WAIT        (optional)  "true" → rollout status each workload.
#   PLUGIN_WAIT_TIMEOUT(optional)  per-workload timeout (default 120s).
#   PLUGIN_NAMESPACE / PLUGIN_ENSURE_NAMESPACE
#   PLUGIN_SERVER_SIDE / PLUGIN_ENABLE_HELM
#   PLUGIN_PRUNE / PLUGIN_PRUNE_LABEL
#   PLUGIN_KUBECONFIG  (optional)  path / inline / base64 (cluster registry).
#   PLUGIN_EXTRA_ARGS  (optional)  forwarded verbatim to kubectl apply.
#
# Same kubeconfig discovery as helm/kubectl plugins so operators juggle
# one convention. `gettext` provides envsubst for PLUGIN_ENVSUBST.
# Base alpine:3.20 (EOL 2026-04-01) -> 3.24.4 (pinned) + apk upgrade. Both tools
# bumped off an old Go (Go-stdlib CVE-2025-68121): kustomize 5.4.3 -> 5.8.3,
# kubectl v1.31.1 -> v1.37.2 (both on a patched Go). Both downloads are now
# architecture-aware (TARGETARCH amd64/arm64) and VERIFIED against the upstream
# sha256 (kustomize checksums.txt / dl.k8s.io .sha256) — the old `curl` had no
# integrity check and hardcoded amd64. NOTE: plugins.yml publishes `:v1`
# linux/amd64 only (fleet-wide); this Dockerfile builds correctly for arm64 once
# that is enabled. kubectl skew: supported within +/-1 minor of the API server.
FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
ARG TARGETARCH=amd64

ENV KUSTOMIZE_VERSION=5.8.3
ENV KUBECTL_VERSION=v1.37.2

RUN apk upgrade --no-cache && apk add --no-cache bash ca-certificates curl gettext \
    && case "${TARGETARCH}" in \
         amd64) KUST_SHA=cb9e31198d3f63b44848bd0afc4c8efc56e6cfef9c93a4a39a211940e6decd61; \
                KCTL_SHA=c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4 ;; \
         arm64) KUST_SHA=9867b76482cfc6d25b546abc1d2b5a32bd137505aba640649b4b378f64f6d68f; \
                KCTL_SHA=fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509 ;; \
         *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fSL -o /tmp/kustomize.tgz \
         "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz" \
    && echo "${KUST_SHA}  /tmp/kustomize.tgz" | sha256sum -c - \
    && tar -xz -C /usr/local/bin -f /tmp/kustomize.tgz kustomize \
    && rm /tmp/kustomize.tgz && chmod +x /usr/local/bin/kustomize \
    && curl -fSL -o /usr/local/bin/kubectl \
         "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
    && echo "${KCTL_SHA}  /usr/local/bin/kubectl" | sha256sum -c - \
    && chmod +x /usr/local/bin/kubectl

COPY entrypoint.sh /usr/local/bin/gocdnext-kustomize
RUN chmod +x /usr/local/bin/gocdnext-kustomize

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-kustomize"]
