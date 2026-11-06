# gocdnext/kaniko — rootless container build. The complement to
# gocdnext/docker: no daemon required, ideal for the Kubernetes
# engine where DinD is heavy. Kaniko runs inside a userspace
# container and writes the built image straight to a remote
# registry (no local daemon, no socket, no privileged mode).
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_IMAGE         (required)  target tag, e.g. "ghcr.io/org/app:v1".
#                                    Must be fully qualified — kaniko
#                                    writes straight to the registry.
#   PLUGIN_DOCKERFILE    (optional)  Dockerfile path. Defaults to
#                                    "Dockerfile" inside the build
#                                    context.
#   PLUGIN_CONTEXT       (optional)  build context dir. Defaults to
#                                    ".".
#   PLUGIN_BUILD_ARGS    (optional)  comma- or newline-separated
#                                    KEY=VALUE list.
#   PLUGIN_CACHE         (optional)  "true"/"false" (default "false").
#                                    When true, kaniko caches layers
#                                    under $PLUGIN_IMAGE-cache.
#   PLUGIN_REGISTRY      (optional)  registry hostname for auth.
#                                    Defaults to the prefix of
#                                    PLUGIN_IMAGE up to first "/".
#   PLUGIN_USERNAME      (optional)  registry user. Empty = anonymous
#                                    (works for public push-less
#                                    builds in PR previews via
#                                    push=false).
#   PLUGIN_PASSWORD      (optional)  registry token. Pipe from
#                                    `secrets:` so it never lands
#                                    in the log.
#   PLUGIN_PUSH          (optional)  "true"/"false". Defaults to
#                                    "true". When false, kaniko
#                                    builds the image WITHOUT
#                                    publishing it — useful on PR
#                                    previews.
#
# AUTHORITY CHANGE: GoogleContainerTools/kaniko was archived (2025-06) and its
# last release ships Go stdlib + grpc CRITICALs with no future fix. We move to
# the maintained community fork `osscontainertools/kaniko` — active, publishing
# public cosign-signed images — pinned to v1.29.0-debug by digest. Trivy
# --ignore-unfixed on this image: 0 CRITICAL / 0 HIGH. Still a distroless image
# whose only shell is /busybox/sh (no bash); the entrypoint targets it.
FROM ghcr.io/osscontainertools/kaniko:v1.29.0-debug@sha256:350929376b8a69afb1d8b362086c809e4ef6799fa1bcb317c2806ae1447b3845

# /kaniko/executor and /busybox/sh are preserved by the fork. The entrypoint
# builds its argv entirely in-shell (no temp file), so no writable /tmp is
# needed.
COPY entrypoint.sh /kaniko/gocdnext-kaniko
RUN ["/busybox/sh","-c","chmod +x /kaniko/gocdnext-kaniko"]

ENTRYPOINT ["/kaniko/gocdnext-kaniko"]
