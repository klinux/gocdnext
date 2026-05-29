#!/bin/bash
# gocdnext/buildx — container build via docker buildx.
# See Dockerfile for the full input contract.

set -euo pipefail

# trim removes leading + trailing whitespace (including the trailing
# newline YAML's `|` block-scalar leaves on every value). Buildx
# parses `--platform "linux/amd64\n"` as a single platform whose
# name has a trailing newline, then trips on `lstat platform` when
# resolving the build context. Trim once at the boundary so every
# subsequent var read is clean.
trim() {
    local s="${1-}"
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    printf '%s' "$s"
}

PLUGIN_IMAGE="$(trim "${PLUGIN_IMAGE:-}")"
if [ -z "${PLUGIN_IMAGE}" ]; then
    echo "gocdnext/buildx: PLUGIN_IMAGE is required" >&2
    echo "  example: image: ghcr.io/org/app" >&2
    exit 2
fi

DOCKERFILE="$(trim "${PLUGIN_DOCKERFILE:-Dockerfile}")"
CONTEXT="$(trim "${PLUGIN_CONTEXT:-.}")"
PUSH="$(trim "${PLUGIN_PUSH:-true}")"
# Default is amd64-only since v0.4.12 — multi-arch QEMU emulation on
# x86 runners adds 3-5x build time and triggers a privileged `docker
# run` for binfmt, which security-conscious clusters reject. Users
# who actually need cross-arch declare `platforms: linux/amd64,linux/arm64`
# explicitly and pay the QEMU cost knowingly.
PLATFORMS="$(trim "${PLUGIN_PLATFORMS:-linux/amd64}")"

tags_raw="$(trim "${PLUGIN_TAGS:-latest}")"
tags_raw="${tags_raw//,/ }"
read -ra TAGS <<<"${tags_raw}"

# Wait for the Docker daemon before issuing any `docker run` /
# `docker buildx` against it. The agent's k8s engine adds a DinD
# sidecar when the YAML job declares `docker: true`, and DinD takes
# ~1-2s to listen on tcp://localhost:2375. The convention (per the
# agent's kubernetes.go comment) is "the daemon-readiness check
# belongs in user code" — so we own it here. Up to 60s; longer waits
# usually indicate DinD failed to start at all (image pull, privileged
# denied by PSP) and a clear timeout error is more useful than a
# silent hang.
echo "==> waiting for docker daemon"
for i in $(seq 1 60); do
    if docker info >/dev/null 2>&1; then
        echo "==> docker daemon ready after ${i}s"
        break
    fi
    if [ "${i}" = "60" ]; then
        echo "gocdnext/buildx: docker daemon did not become reachable within 60s" >&2
        echo "  DOCKER_HOST=${DOCKER_HOST:-<unset>}" >&2
        echo "  is \`docker: true\` set on the job? agent wires a DinD sidecar only then" >&2
        exit 2
    fi
    sleep 1
done

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
# build steps. ONLY runs when the target platforms include something
# other than the host arch: pulling the binfmt image + a privileged
# `docker run` for nothing wastes 10-30s + offends PodSecurity
# policies that block privileged containers cluster-wide.
case "$(uname -m)" in
    x86_64)  host_platform=linux/amd64 ;;
    aarch64) host_platform=linux/arm64 ;;
    armv7l)  host_platform=linux/arm/v7 ;;
    *)       host_platform="linux/$(uname -m)" ;;
esac
need_qemu=0
IFS=',' read -ra _platforms <<<"${PLATFORMS}"
for p in "${_platforms[@]}"; do
    p="$(trim "$p")"
    if [ -n "$p" ] && [ "$p" != "$host_platform" ]; then
        need_qemu=1
        break
    fi
done
if [ "${need_qemu}" = "1" ]; then
    echo "==> registering QEMU handlers (binfmt) — cross-arch target"
    docker run --privileged --rm tonistiigi/binfmt --install all >/dev/null
else
    echo "==> native build (${host_platform}), skipping QEMU"
fi

# Pre-detect whether the layer cache backend points at a non-AWS
# S3-compatible endpoint (GCS interop, MinIO, R2, etc.). Recent
# aws-sdk-go-v2 (which BuildKit uses internally) sends
# x-amz-checksum-* headers on PutObject by default; non-AWS
# endpoints don't recognise those headers, include them in the v4
# signature canonical request, and reject the call as
# SignatureDoesNotMatch (surfaces upstream as a generic 403). The
# SDK reads AWS_REQUEST_CHECKSUM_CALCULATION / _RESPONSE_VALIDATION
# from the BuildKit container's env to opt out — we propagate it
# via `docker buildx create --driver-opt env.NAME=VALUE` so the fix
# lands inside BuildKit itself, not just the plugin shell.
needs_no_checksum=0
if [ "${PLUGIN_CACHE:-}" = "bucket" ]; then
    _backend_for_probe="$(trim "${GOCDNEXT_LAYER_CACHE_BACKEND:-s3}")"
    _endpoint_for_probe="$(trim "${GOCDNEXT_LAYER_CACHE_ENDPOINT:-}")"
    case "${_backend_for_probe}" in
        gcs|gs)
            needs_no_checksum=1
            ;;
        *)
            # Backend == s3 but with a custom endpoint URL → not
            # AWS S3 native; opt out defensively.
            [ -n "${_endpoint_for_probe}" ] && needs_no_checksum=1
            ;;
    esac
fi

# A fresh builder per run keeps cache isolated and avoids a
# "default" builder that already exists in some docker-in-docker
# setups from being reused in a way that skips the QEMU wiring.
BUILDER="gocdnext-${RANDOM}"
trap 'docker buildx rm "${BUILDER}" >/dev/null 2>&1 || true' EXIT
buildx_create_args=(--name "${BUILDER}" --use)
# When the operator sets PLUGIN_BUILDKIT_IMAGE explicitly we honour
# it as-is. Otherwise, the default fork below picks a sensible
# image for the cache backend in play.
buildkit_image="$(trim "${PLUGIN_BUILDKIT_IMAGE:-}")"
if [ "${needs_no_checksum}" = "1" ]; then
    echo "==> buildkit: disabling AWS SDK auto-checksum (non-AWS S3 endpoint detected)"
    buildx_create_args+=(--driver-opt "env.AWS_REQUEST_CHECKSUM_CALCULATION=when_required")
    buildx_create_args+=(--driver-opt "env.AWS_RESPONSE_CHECKSUM_VALIDATION=when_required")
    if [ -z "${buildkit_image}" ]; then
        # `moby/buildkit:buildx-stable-1` (v0.18.x) ships an
        # aws-sdk-go-v2 < v1.30 that doesn't read
        # AWS_REQUEST_CHECKSUM_CALCULATION — setting the env on the
        # container is a no-op there. Pinning v0.20.2 (Feb 2025)
        # gets a modern SDK that honours the opt-out. The version
        # is intentionally explicit (not `latest`) so a future
        # upstream regression can't silently break GCS/MinIO/R2
        # cache for every gocdnext install at once.
        buildkit_image="moby/buildkit:v0.20.2"
        echo "==> buildkit: pinning ${buildkit_image} for a modern aws-sdk-go-v2"
    fi
fi

# AWS credentials live in the PLUGIN container's env (injected by
# the runner profile's `secrets:` list). The BuildKit container is
# a SEPARATE process — `docker buildx create` spawns it with
# whatever env the operator passed via --driver-opt; nothing else
# crosses the boundary. Without explicit propagation BuildKit's S3
# cache backend has no AWS_ACCESS_KEY_ID at all and fails with the
# tell-tale empty-RequestID 403:
#   StatusCode: 403, RequestID: , HostID: , api error Forbidden
# (no GCS round-trip happened — credential resolution gave up
# before the SDK could even build a signed HEAD).
#
# Propagate the standard AWS env trio so BuildKit's S3 client picks
# them up via the same default credential chain it would on a fresh
# CI runner. AWS_SESSION_TOKEN is forwarded when present for SSO /
# STS setups; for GCS HMAC the key+secret pair is all that matters.
# We do it only when cache uses a bucket — for `registry`/`inline`
# the BuildKit container has no use for AWS env and we'd be
# leaking an env scope it doesn't need.
if [ "${PLUGIN_CACHE:-}" = "bucket" ]; then
    if [ -n "${AWS_ACCESS_KEY_ID:-}" ]; then
        buildx_create_args+=(--driver-opt "env.AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}")
    fi
    if [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
        buildx_create_args+=(--driver-opt "env.AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}")
    fi
    if [ -n "${AWS_SESSION_TOKEN:-}" ]; then
        buildx_create_args+=(--driver-opt "env.AWS_SESSION_TOKEN=${AWS_SESSION_TOKEN}")
    fi
    if [ -n "${AWS_ACCESS_KEY_ID:-}" ]; then
        echo "==> buildkit: forwarding AWS credentials for cache backend"
    fi
fi

if [ -n "${buildkit_image}" ]; then
    buildx_create_args+=(--driver-opt "image=${buildkit_image}")
fi
docker buildx create "${buildx_create_args[@]}" >/dev/null
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
            # GCS is supported via S3-interop (https://cloud.google.com/storage/docs/interoperability).
            # BuildKit has no native gcs cache type, so when the operator sets
            # GOCDNEXT_LAYER_CACHE_BACKEND=gcs we translate to type=s3 +
            # storage.googleapis.com endpoint behind the scenes. The HMAC
            # access-key / secret pair must reach the container via the
            # runner profile's `secrets:` as AWS_ACCESS_KEY_ID +
            # AWS_SECRET_ACCESS_KEY (BuildKit's s3 backend reads them from
            # the standard AWS env names regardless of the actual provider).
            backend_raw="$(trim "${GOCDNEXT_LAYER_CACHE_BACKEND:-s3}")"
            backend="${backend_raw}"
            endpoint="$(trim "${GOCDNEXT_LAYER_CACHE_ENDPOINT:-}")"
            case "${backend_raw}" in
                gcs|gs)
                    backend="s3"
                    endpoint="${endpoint:-https://storage.googleapis.com}"
                    # GCS doesn't use AWS regions; "auto" is the standard
                    # placeholder that keeps BuildKit's s3 client happy
                    # without imposing a fake region on the operator.
                    region="${GOCDNEXT_LAYER_CACHE_REGION:-auto}"
                    if [ -z "${AWS_ACCESS_KEY_ID:-}${AWS_SECRET_ACCESS_KEY:-}" ]; then
                        echo "gocdnext/buildx: cache: gcs backend requires AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (GCS HMAC keys); declare them in the job's secrets: or runner profile" >&2
                        exit 2
                    fi
                    ;;
                s3|"")
                    backend="s3"
                    region="${GOCDNEXT_LAYER_CACHE_REGION:-${AWS_REGION:-us-east-1}}"
                    ;;
                *)
                    region="${GOCDNEXT_LAYER_CACHE_REGION:-${AWS_REGION:-us-east-1}}"
                    ;;
            esac
            # Cache key namespaces by image repo so multiple
            # images can share one bucket without colliding.
            name="${GOCDNEXT_LAYER_CACHE_NAME:-${PLUGIN_IMAGE}}"
            spec="type=${backend},region=${region},bucket=${GOCDNEXT_LAYER_CACHE_BUCKET},name=${name}"
            if [ -n "${endpoint}" ]; then
                spec="${spec},endpoint_url=${endpoint}"
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

# Compose the final invocation in one array so we can `printf` it
# back to the operator before exec'ing. When buildx fails with a
# cryptic `resolve : lstat X` (almost always a stray-whitespace input
# the shell didn't expand the way the operator assumed), the printed
# command makes the actual argv visible without `set -x` ceremony.
final_cmd=(docker buildx build
    --platform "${PLATFORMS}"
    --file "${DOCKERFILE}"
    "${tag_args[@]}"
    "${build_arg_args[@]}"
    "${cache_args[@]}"
    "${push_args[@]}"
    "${CONTEXT}")
echo "==> docker buildx build --platform ${PLATFORMS} (${#TAGS[@]} tag(s))"
printf '    %q' "${final_cmd[@]}"
printf '\n'
"${final_cmd[@]}"

if [ "${PUSH}" != "true" ]; then
    echo "==> push skipped (PLUGIN_PUSH=${PUSH}). Multi-arch images stay in the builder cache only."
fi
