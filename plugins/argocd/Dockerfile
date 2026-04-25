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
#   PLUGIN_GRPC_WEB       (optional)  "true" → --grpc-web (use
#                                     when Argo is behind an L7
#                                     reverse proxy that doesn't
#                                     speak HTTP/2 to backends).
#
# The CLI auths via the token in env (ARGOCD_AUTH_TOKEN) — no
# config file persistence needed, every run is fresh.
FROM alpine:3.20

ENV ARGOCD_VERSION=v2.13.1

RUN apk add --no-cache bash ca-certificates curl \
    && curl -sSL -o /usr/local/bin/argocd \
       "https://github.com/argoproj/argo-cd/releases/download/${ARGOCD_VERSION}/argocd-linux-amd64" \
    && chmod +x /usr/local/bin/argocd

COPY entrypoint.sh /usr/local/bin/gocdnext-argocd
RUN chmod +x /usr/local/bin/gocdnext-argocd

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-argocd"]
