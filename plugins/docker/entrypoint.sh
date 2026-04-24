#!/bin/bash
# gocdnext/docker — build + push container image.
#
# See Dockerfile for the full input contract.

set -euo pipefail

if [ -z "${PLUGIN_IMAGE:-}" ]; then
    echo "gocdnext/docker: PLUGIN_IMAGE is required" >&2
    echo "  example: image: ghcr.io/org/app" >&2
    exit 2
fi

DOCKERFILE="${PLUGIN_DOCKERFILE:-Dockerfile}"
CONTEXT="${PLUGIN_CONTEXT:-.}"
PUSH="${PLUGIN_PUSH:-true}"

# Tag list: accept comma or whitespace between entries so users
# can write `tags: latest, v1` or `tags: "latest v1"` and both
# work. Defaults to "latest" when unset.
tags_raw="${PLUGIN_TAGS:-latest}"
tags_raw="${tags_raw//,/ }"
read -ra TAGS <<<"${tags_raw}"

# Registry login — infer host from PLUGIN_IMAGE when not set
# (`ghcr.io/org/app` → `ghcr.io`). Skip entirely when there's no
# username; public pushes to a registry that doesn't require
# auth still work, and anonymous pull-only builds trivially.
if [ -n "${PLUGIN_USERNAME:-}" ]; then
    registry="${PLUGIN_REGISTRY:-}"
    if [ -z "${registry}" ]; then
        # `image/path:tag` → strip everything after the first /
        # to recover the host. Fallback to docker.io when no / is
        # present (shouldn't happen in practice — operators use
        # fully-qualified refs — but we don't want a crash).
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

# Accumulate -t flags so one build produces every tag — saves
# the extra layer unpacks that sequential `docker tag + push`
# would incur.
tag_args=()
for t in "${TAGS[@]}"; do
    tag_args+=("-t" "${PLUGIN_IMAGE}:${t}")
done

# Build args: newlines or commas as separators. KEY=VALUE only;
# anything else is skipped with a warning so a typo doesn't
# silently ship a missing build arg.
build_arg_args=()
if [ -n "${PLUGIN_BUILD_ARGS:-}" ]; then
    ba_raw="${PLUGIN_BUILD_ARGS//,/$'\n'}"
    while IFS= read -r line; do
        line="${line## }"
        line="${line%% }"
        [ -z "${line}" ] && continue
        if [[ "${line}" != *"="* ]]; then
            echo "gocdnext/docker: skipping malformed build-arg (no =): ${line}" >&2
            continue
        fi
        build_arg_args+=("--build-arg" "${line}")
    done <<<"${ba_raw}"
fi

echo "==> docker build ${CONTEXT} -f ${DOCKERFILE} (${#TAGS[@]} tag(s))"
docker build \
    --file "${DOCKERFILE}" \
    "${tag_args[@]}" \
    "${build_arg_args[@]}" \
    "${CONTEXT}"

if [ "${PUSH}" = "true" ]; then
    for t in "${TAGS[@]}"; do
        echo "==> docker push ${PLUGIN_IMAGE}:${t}"
        docker push "${PLUGIN_IMAGE}:${t}"
    done
else
    echo "==> push skipped (PLUGIN_PUSH=${PUSH})"
fi
