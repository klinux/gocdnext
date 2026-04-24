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
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates curl git \
    && curl -sSL https://get.helm.sh/helm-v3.16.2-linux-amd64.tar.gz \
       | tar -xz -C /tmp \
    && mv /tmp/linux-amd64/helm /usr/local/bin/helm \
    && rm -rf /tmp/linux-amd64 \
    && chmod +x /usr/local/bin/helm

COPY entrypoint.sh /usr/local/bin/gocdnext-helm
RUN chmod +x /usr/local/bin/gocdnext-helm

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-helm"]
