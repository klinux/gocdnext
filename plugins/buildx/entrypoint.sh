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

echo "==> docker buildx build --platform ${PLATFORMS} (${#TAGS[@]} tag(s))"
docker buildx build \
    --platform "${PLATFORMS}" \
    --file "${DOCKERFILE}" \
    "${tag_args[@]}" \
    "${build_arg_args[@]}" \
    "${push_args[@]}" \
    "${CONTEXT}"

if [ "${PUSH}" != "true" ]; then
    echo "==> push skipped (PLUGIN_PUSH=${PUSH}). Multi-arch images stay in the builder cache only."
fi
