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
# Base pinned by version+digest. docker:24-cli still shipped fixable OS
# CRITICALs (Trivy --ignore-unfixed); bumping to 29.9.3-cli + `apk upgrade`
# clears them (0 fixable CRITICAL; residual HIGH classified in CHANGELOG).
# docker:29.9.3-cli is the standalone Docker CLI — `docker build`/`docker push`
# talk to whatever daemon `docker: true` wires in (DinD sidecar / mounted
# socket). The CLI negotiates the daemon's API version, so this 29 CLI drives
# the runtime's default docker:24-dind daemon fine (proven API 1.43 end-to-end
# in smoke_test.sh). `docker build` on the 29 CLI uses the daemon's integrated
# BuildKit — no separate buildx binary is needed for single-arch builds, also
# proven against a docker:24 DinD daemon. Multi-arch belongs to gocdnext/buildx.
#
# NOTE: `apk upgrade` pulls from Alpine's MUTABLE package repos, so a rebuild
# weeks later can resolve newer patch packages than this one did — the FROM
# digest pins the base layer, NOT the apk-upgraded set. Intended trade-off:
# a reproducible base plus always-current OS security patches.
FROM docker:29.9.3-cli@sha256:2357639ad1ea7d0194134bf3292622fd4e562483966ed674e5d7c945b5f3b28d

RUN apk upgrade --no-cache && apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-docker
RUN chmod +x /usr/local/bin/gocdnext-docker

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-docker"]
