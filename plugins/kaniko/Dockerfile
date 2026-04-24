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
#                                    builds into an in-memory
#                                    tarball and exits — useful
#                                    on PR previews.
#
# Kaniko runs in a distroless container without a shell; we layer
# a BusyBox shell in so the entrypoint can do its env plumbing.
FROM gcr.io/kaniko-project/executor:v1.23.2-debug

# The debug image has /busybox/sh but no `bash`; the entrypoint
# targets /busybox/sh explicitly.
COPY entrypoint.sh /kaniko/gocdnext-kaniko
RUN chmod +x /kaniko/gocdnext-kaniko

ENTRYPOINT ["/kaniko/gocdnext-kaniko"]
