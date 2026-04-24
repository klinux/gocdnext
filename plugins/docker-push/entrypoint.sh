#!/bin/bash
# gocdnext/docker-push — login + retag + push without building.
# See Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_SOURCE:-}" ]; then
    echo "gocdnext/docker-push: PLUGIN_SOURCE is required" >&2
    exit 2
fi
if [ -z "${PLUGIN_TARGET:-}" ]; then
    echo "gocdnext/docker-push: PLUGIN_TARGET is required" >&2
    exit 2
fi

PULL="${PLUGIN_PULL:-false}"

tags_raw="${PLUGIN_TAGS:-latest}"
tags_raw="${tags_raw//,/ }"
read -ra TAGS <<<"${tags_raw}"

if [ -n "${PLUGIN_USERNAME:-}" ]; then
    registry="${PLUGIN_REGISTRY:-}"
    if [ -z "${registry}" ]; then
        if [[ "${PLUGIN_TARGET}" == */* ]]; then
            registry="${PLUGIN_TARGET%%/*}"
        else
            registry="docker.io"
        fi
    fi
    echo "==> logging into ${registry} as ${PLUGIN_USERNAME}"
    echo "${PLUGIN_PASSWORD:-}" | docker login "${registry}" \
        --username "${PLUGIN_USERNAME}" --password-stdin
fi

if [ "${PULL}" = "true" ]; then
    echo "==> docker pull ${PLUGIN_SOURCE}"
    docker pull "${PLUGIN_SOURCE}"
fi

for t in "${TAGS[@]}"; do
    echo "==> docker tag ${PLUGIN_SOURCE} ${PLUGIN_TARGET}:${t}"
    docker tag "${PLUGIN_SOURCE}" "${PLUGIN_TARGET}:${t}"
    echo "==> docker push ${PLUGIN_TARGET}:${t}"
    docker push "${PLUGIN_TARGET}:${t}"
done
