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
    # The config holds base64("user:password") — reversible — so lock it down to
    # the owner before writing the credential into it.
    chmod 700 /kaniko/.docker
    auth=$(printf '%s:%s' "${PLUGIN_USERNAME}" "${PLUGIN_PASSWORD:-}" | base64 -w0 2>/dev/null || \
           printf '%s:%s' "${PLUGIN_USERNAME}" "${PLUGIN_PASSWORD:-}" | base64)
    # Single-registry config; kaniko re-uses this entry for any
    # image whose prefix matches. Multi-registry pushes need two
    # plugin calls, same as gocdnext/docker.
    cat >/kaniko/.docker/config.json <<EOF
{"auths":{"${registry}":{"auth":"${auth}"}}}
EOF
    chmod 600 /kaniko/.docker/config.json
fi

set -- \
    --dockerfile="${CONTEXT}/${DOCKERFILE}" \
    --context="dir://${CONTEXT}" \
    --destination="${PLUGIN_IMAGE}"

if [ "${PUSH}" != "true" ]; then
    set -- "$@" --no-push
fi

if [ "${PLUGIN_CACHE:-false}" = "true" ]; then
    # --cache-repo must be a bare repository — no tag or digest. Strip them from
    # PLUGIN_IMAGE before appending -cache, or kaniko rejects the cache repo
    # ("repository can only contain...") and SILENTLY skips caching at exit 0.
    cache_image="${PLUGIN_IMAGE%%@*}"            # drop any @sha256:… digest
    case "${cache_image##*/}" in
        *:*) cache_image="${cache_image%:*}" ;;  # drop a :tag on the last path segment only
    esac
    set -- "$@" --cache=true --cache-repo="${cache_image}-cache"
fi

# Build args — comma- or newline-separated. Each KEY=VALUE becomes one
# `--build-arg KEY=VALUE` positional. Built in the CURRENT shell via a here-doc
# (NOT a pipe-into-while subshell), so `set --` survives AND a VALUE containing
# spaces stays one argument — the old temp-file + word-split approach corrupted
# `MSG=hello world` into two args. No temp file, so no /tmp dependency either.
# Malformed entries (no "=") are skipped with a warn.
if [ -n "${PLUGIN_BUILD_ARGS:-}" ]; then
    normalised=$(printf '%s' "${PLUGIN_BUILD_ARGS}" | tr ',' '\n')
    while IFS= read -r line; do
        line=$(printf '%s' "${line}" | sed 's/^ *//;s/ *$//')
        [ -z "${line}" ] && continue
        case "${line}" in
            *=*) set -- "$@" --build-arg "${line}" ;;
            *)   echo "gocdnext/kaniko: skipping malformed build-arg (no =): ${line}" >&2 ;;
        esac
    done <<EOF
${normalised}
EOF
fi

exec /kaniko/executor "$@"
