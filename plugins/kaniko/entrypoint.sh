#!/busybox/sh
# gocdnext/kaniko — rootless container build. See Dockerfile for
# the full input contract.
#
# Busybox /bin/sh — no bashisms. Arrays don't exist; arg lists
# are built with `set --` + positional params.

set -eu

if [ -z "${PLUGIN_IMAGE:-}" ]; then
    echo "gocdnext/kaniko: PLUGIN_IMAGE is required" >&2
    echo "  example: image: ghcr.io/org/app:v1" >&2
    exit 2
fi

DOCKERFILE="${PLUGIN_DOCKERFILE:-Dockerfile}"
CONTEXT="${PLUGIN_CONTEXT:-.}"
PUSH="${PLUGIN_PUSH:-true}"

# Registry login — kaniko reads credentials from
# /kaniko/.docker/config.json. Synthesise one from username /
# password when provided. Base64 the "user:password" per the
# docker config schema.
if [ -n "${PLUGIN_USERNAME:-}" ]; then
    registry="${PLUGIN_REGISTRY:-}"
    if [ -z "${registry}" ]; then
        case "${PLUGIN_IMAGE}" in
            */*) registry=$(printf '%s' "${PLUGIN_IMAGE}" | cut -d/ -f1) ;;
            *)   registry="index.docker.io" ;;
        esac
    fi
    mkdir -p /kaniko/.docker
    auth=$(printf '%s:%s' "${PLUGIN_USERNAME}" "${PLUGIN_PASSWORD:-}" | base64 -w0 2>/dev/null || \
           printf '%s:%s' "${PLUGIN_USERNAME}" "${PLUGIN_PASSWORD:-}" | base64)
    # Single-registry config; kaniko re-uses this entry for any
    # image whose prefix matches. Multi-registry pushes need two
    # plugin calls, same as gocdnext/docker.
    cat >/kaniko/.docker/config.json <<EOF
{"auths":{"${registry}":{"auth":"${auth}"}}}
EOF
fi

set -- \
    --dockerfile="${CONTEXT}/${DOCKERFILE}" \
    --context="dir:///workspace/${CONTEXT}" \
    --destination="${PLUGIN_IMAGE}"

if [ "${PUSH}" != "true" ]; then
    set -- "$@" --no-push
fi

if [ "${PLUGIN_CACHE:-false}" = "true" ]; then
    set -- "$@" --cache=true --cache-repo="${PLUGIN_IMAGE}-cache"
fi

# Build args — comma- or newline-separated. Each entry becomes
# one --build-arg KEY=VALUE. Malformed entries (no "=") are
# skipped with a warn so a typo doesn't silently ship a missing
# value.
if [ -n "${PLUGIN_BUILD_ARGS:-}" ]; then
    # Normalise separators to newline, then iterate.
    normalised=$(printf '%s' "${PLUGIN_BUILD_ARGS}" | tr ',' '\n')
    printf '%s\n' "${normalised}" | while IFS= read -r line; do
        line=$(printf '%s' "${line}" | sed 's/^ *//;s/ *$//')
        [ -z "${line}" ] && continue
        case "${line}" in
            *=*) : ;;
            *)
                echo "gocdnext/kaniko: skipping malformed build-arg (no =): ${line}" >&2
                continue
                ;;
        esac
        # Trick: build-args must be passed to kaniko one-shot,
        # but we're inside a subshell loop so `set --` here
        # wouldn't survive. Instead, append to a temp file and
        # re-exec from the parent scope.
        printf -- '--build-arg\n%s\n' "${line}" >>/tmp/kaniko-build-args
    done
    if [ -s /tmp/kaniko-build-args ]; then
        # Rebuild positional args with the collected build-args
        # at the end. Can't use arrays (busybox) so re-construct
        # a single command line.
        extra_args=""
        while IFS= read -r a; do
            extra_args="${extra_args} ${a}"
        done </tmp/kaniko-build-args
        # shellcheck disable=SC2086
        exec /kaniko/executor "$@" ${extra_args}
    fi
fi

exec /kaniko/executor "$@"
