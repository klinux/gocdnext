# gocdnext/buildx — multi-arch container build + push via
# `docker buildx`. Unlike `gocdnext/docker` (single-arch, one
# platform), buildx sets up a QEMU-emulated cross-builder so a
# single job produces amd64 + arm64 images and pushes them as a
# manifest list.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_IMAGE      (required)  target repo, e.g. "ghcr.io/org/app".
#   PLUGIN_TAGS       (optional)  comma/space-separated tags. Default "latest".
#   PLUGIN_PLATFORMS  (optional)  comma-separated. Default
#                                 "linux/amd64,linux/arm64" — the
#                                 2 platforms 95% of images ship
#                                 today. Add arm/v7, ppc64le,
#                                 s390x when needed.
#   PLUGIN_DOCKERFILE (optional)  default "Dockerfile".
#   PLUGIN_CONTEXT    (optional)  default ".".
#   PLUGIN_BUILD_ARGS (optional)  KEY=VALUE, comma- or newline-sep.
#   PLUGIN_PUSH       (optional)  "true"/"false". Default "true".
#                                 "false" builds the cross images
#                                 into the builder cache but does
#                                 NOT pull them into the local
#                                 daemon — multi-arch manifests
#                                 can't live in the legacy store.
#   PLUGIN_REGISTRY   (optional)  login host. Default: prefix of
#                                 PLUGIN_IMAGE up to the first "/".
#   PLUGIN_USERNAME   (optional)  registry user. Empty = skip login.
#   PLUGIN_PASSWORD   (optional)  piped from secrets:.
#
# Requires `docker: true` on the job. The bundled image ships
# buildx + binfmt setup so the job can `exec docker buildx`
# against the daemon the agent wired in.
FROM docker:24-cli

RUN apk add --no-cache bash docker-cli-buildx

COPY entrypoint.sh /usr/local/bin/gocdnext-buildx
RUN chmod +x /usr/local/bin/gocdnext-buildx

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-buildx"]
