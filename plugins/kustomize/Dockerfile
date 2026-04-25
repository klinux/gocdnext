# gocdnext/kustomize — kustomize render + apply wrapper. Pairs
# with gocdnext/kubectl + gocdnext/helm for the deploy half of a
# pipeline: build manifests from a kustomization tree, optionally
# pipe them straight into `kubectl apply`.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PATH        (required)  Workspace-relative path to the
#                                  kustomization root (folder
#                                  containing kustomization.yaml).
#   PLUGIN_ACTION      (optional)  apply | build | diff (default apply)
#                                    apply  → render + kubectl apply
#                                    build  → render to stdout only
#                                    diff   → render + kubectl diff
#   PLUGIN_KUBECONFIG  (optional)  path / inline YAML / base64 YAML.
#                                  Auto-detected like helm/kubectl
#                                  plugins. Empty defers to the
#                                  agent's in-cluster service account.
#   PLUGIN_NAMESPACE   (optional)  Default namespace for kubectl
#                                  apply (`-n`). Manifests with an
#                                  explicit ns win.
#   PLUGIN_PRUNE       (optional)  "true" → kubectl apply --prune
#                                  with a label selector (set
#                                  PLUGIN_PRUNE_LABEL).
#   PLUGIN_PRUNE_LABEL (optional)  Label selector required when
#                                  PRUNE=true. Example:
#                                  "app.kubernetes.io/managed-by=gocdnext"
#   PLUGIN_EXTRA_ARGS  (optional)  Forwarded verbatim to kubectl
#                                  (e.g. "--server-side --force-conflicts").
#
# Same kubeconfig discovery as helm/kubectl plugins so operators
# juggle one convention.
FROM alpine:3.20

ENV KUSTOMIZE_VERSION=5.4.3
ENV KUBECTL_VERSION=v1.31.1

RUN apk add --no-cache bash ca-certificates curl \
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
