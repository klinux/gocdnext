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
FROM alpine:3.20

ENV KUSTOMIZE_VERSION=5.4.3
ENV KUBECTL_VERSION=v1.31.1

RUN apk add --no-cache bash ca-certificates curl gettext \
    && curl -sSL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_amd64.tar.gz" \
       | tar -xz -C /tmp \
    && mv /tmp/kustomize /usr/local/bin/kustomize \
    && chmod +x /usr/local/bin/kustomize \
    && curl -sSL -o /usr/local/bin/kubectl \
       "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl

COPY entrypoint.sh /usr/local/bin/gocdnext-kustomize
RUN chmod +x /usr/local/bin/gocdnext-kustomize

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-kustomize"]
