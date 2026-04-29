#!/bin/bash
# gocdnext/buildx — multi-arch container build via docker buildx.
# See Dockerfile for the full input contract.

set -euo pipefail

if [ -z "${PLUGIN_IMAGE:-}" ]; then
    echo "gocdnext/buildx: PLUGIN_IMAGE is required" >&2
    echo "  example: image: ghcr.io/org/app" >&2
    exit 2
fi

DOCKERFILE="${PLUGIN_DOCKERFILE:-Dockerfile}"
CONTEXT="${PLUGIN_CONTEXT:-.}"
PUSH="${PLUGIN_PUSH:-true}"
PLATFORMS="${PLUGIN_PLATFORMS:-linux/amd64,linux/arm64}"

tags_raw="${PLUGIN_TAGS:-latest}"
tags_raw="${tags_raw//,/ }"
read -ra TAGS <<<"${tags_raw}"

if [ -n "${PLUGIN_USERNAME:-}" ]; then
    registry="${PLUGIN_REGISTRY:-}"
    if [ -z "${registry}" ]; then
        if [[ "${PLUGIN_IMAGE}" == */* ]]; then
            registry="${PLUGIN_IMAGE%%/*}"
        else
            registry="docker.io"
        fi
    fi
    echo "==> logging into ${registry} as ${PLUGIN_USERNAME}"
    echo "${PLUGIN_PASSWORD:-}" | docker login "${registry}" \
        --username "${PLUGIN_USERNAME}" --password-stdin
fi

# QEMU emulators — tonistiigi/binfmt registers handlers inside
# the running daemon, so a single amd64 builder can execute arm64
# build steps. Idempotent; rerunning costs one image pull and a
# short container lifetime.
echo "==> registering QEMU handlers (binfmt)"
docker run --privileged --rm tonistiigi/binfmt --install all >/dev/null

# A fresh builder per run keeps cache isolated and avoids a
# "default" builder that already exists in some docker-in-docker
# setups from being reused in a way that skips the QEMU wiring.
BUILDER="gocdnext-${RANDOM}"
trap 'docker buildx rm "${BUILDER}" >/dev/null 2>&1 || true' EXIT
docker buildx create --name "${BUILDER}" --use >/dev/null
docker buildx inspect --bootstrap >/dev/null

tag_args=()
for t in "${TAGS[@]}"; do
    tag_args+=("-t" "${PLUGIN_IMAGE}:${t}")
done

build_arg_args=()
if [ -n "${PLUGIN_BUILD_ARGS:-}" ]; then
    ba_raw="${PLUGIN_BUILD_ARGS//,/$'\n'}"
    while IFS= read -r line; do
        line="${line## }"
        line="${line%% }"
        [ -z "${line}" ] && continue
        if [[ "${line}" != *"="* ]]; then
            echo "gocdnext/buildx: skipping malformed build-arg (no =): ${line}" >&2
            continue
        fi
        build_arg_args+=("--build-arg" "${line}")
    done <<<"${ba_raw}"
fi

# --push produces a manifest list in one go; without it buildx
# only leaves output in its cache because multi-arch images can't
# land in the legacy docker image store. That's intentional —
# users who want `PLUGIN_PUSH=false` get a free "does it build"
# check on both architectures without registry writes.
push_args=()
if [ "${PUSH}" = "true" ]; then
    push_args+=("--push")
fi

# Layer cache wiring. Two surfaces:
#   1. Explicit cache-to / cache-from passed verbatim — wins when set.
#   2. `cache: registry|inline|bucket` shorthand. The "bucket"
#      variant reads GOCDNEXT_LAYER_CACHE_* env vars injected by
#      the runner profile, so the YAML stays clean and credentials
#      never appear next to the job spec.
# AWS_*/GOOGLE_* creds for S3/GCS backends come in via the runner
# profile's `secrets:` (the agent injects them into this container's
# env), so BuildKit picks them up from the daemon's process env.
cache_args=()
cache_to="${PLUGIN_CACHE_TO:-}"
cache_from="${PLUGIN_CACHE_FROM:-}"
if [ -z "${cache_to}" ] && [ -z "${cache_from}" ]; then
    case "${PLUGIN_CACHE:-}" in
        registry)
            cache_to="type=registry,ref=${PLUGIN_IMAGE}:buildcache,mode=max"
            cache_from="type=registry,ref=${PLUGIN_IMAGE}:buildcache"
            ;;
        inline)
            cache_to="type=inline"
            ;;
        bucket)
            if [ -z "${GOCDNEXT_LAYER_CACHE_BUCKET:-}" ]; then
                echo "gocdnext/buildx: cache: bucket requires GOCDNEXT_LAYER_CACHE_BUCKET (set it on the runner profile env)" >&2
                exit 2
            fi
            backend="${GOCDNEXT_LAYER_CACHE_BACKEND:-s3}"
            region="${GOCDNEXT_LAYER_CACHE_REGION:-${AWS_REGION:-us-east-1}}"
            # Cache key namespaces by image repo so multiple
            # images can share one bucket without colliding.
            name="${GOCDNEXT_LAYER_CACHE_NAME:-${PLUGIN_IMAGE}}"
            spec="type=${backend},region=${region},bucket=${GOCDNEXT_LAYER_CACHE_BUCKET},name=${name}"
            if [ -n "${GOCDNEXT_LAYER_CACHE_ENDPOINT:-}" ]; then
                spec="${spec},endpoint_url=${GOCDNEXT_LAYER_CACHE_ENDPOINT}"
            fi
            cache_to="${spec},mode=max"
            cache_from="${spec}"
            ;;
        none|"")
            ;;
        *)
            echo "gocdnext/buildx: unknown cache shorthand '${PLUGIN_CACHE}' — use 'registry', 'inline', 'bucket', 'none', or set cache-to/cache-from explicitly" >&2
            exit 2
            ;;
    esac
fi
[ -n "${cache_to}"   ] && cache_args+=("--cache-to"   "${cache_to}")
[ -n "${cache_from}" ] && cache_args+=("--cache-from" "${cache_from}")
if [ ${#cache_args[@]} -gt 0 ]; then
    echo "==> layer cache: ${cache_args[*]}"
fi

echo "==> docker buildx build --platform ${PLATFORMS} (${#TAGS[@]} tag(s))"
docker buildx build \
    --platform "${PLATFORMS}" \
    --file "${DOCKERFILE}" \
    "${tag_args[@]}" \
    "${build_arg_args[@]}" \
    "${cache_args[@]}" \
    "${push_args[@]}" \
    "${CONTEXT}"

if [ "${PUSH}" != "true" ]; then
    echo "==> push skipped (PLUGIN_PUSH=${PUSH}). Multi-arch images stay in the builder cache only."
fi
