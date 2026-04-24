# gocdnext/docker — build + push container images.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_IMAGE         (required)  target tag, e.g. "ghcr.io/org/app".
#   PLUGIN_TAGS          (optional)  comma- or space-separated tag list.
#                                    Defaults to "latest".
#   PLUGIN_DOCKERFILE    (optional)  path to the Dockerfile. Defaults to
#                                    "Dockerfile" in the build context.
#   PLUGIN_CONTEXT       (optional)  build context dir. Defaults to ".".
#   PLUGIN_BUILD_ARGS    (optional)  newline- or comma-separated
#                                    KEY=VALUE list (forwarded as --build-arg).
#   PLUGIN_PUSH          (optional)  "true"/"false". Defaults to "true".
#   PLUGIN_REGISTRY      (optional)  registry hostname for login. Defaults
#                                    to the prefix of PLUGIN_IMAGE up to
#                                    the first "/". Skip login by leaving
#                                    PLUGIN_USERNAME empty.
#   PLUGIN_USERNAME      (optional)  registry user. Empty = anonymous.
#   PLUGIN_PASSWORD      (optional)  registry token/password. Pair with
#                                    the jobs's `secrets:` list so the
#                                    real value never surfaces in logs.
#
# Requires the job to set `docker: true` in YAML — the agent's
# Docker/K8s engines wire the daemon socket (or DinD sidecar) so
# `docker build` has something to talk to.
FROM docker:24-cli

RUN apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-docker
RUN chmod +x /usr/local/bin/gocdnext-docker

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-docker"]
