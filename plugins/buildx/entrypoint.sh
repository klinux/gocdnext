#!/bin/bash
# gocdnext/buildx — container build via docker buildx.
# See Dockerfile for the full input contract.

set -euo pipefail

# --- Pinned supply-chain images ---------------------------------------------
# Both run with elevated trust: binfmt installs kernel handlers under a
# `docker run --privileged`, and the BuildKit image IS the builder that
# resolves + pushes every layer. docker buildx's own defaults for both are
# FLOATING tags (tonistiigi/binfmt:latest, moby/buildkit:buildx-stable-1) — a
# cold run weeks apart can boot a different privileged image / builder with no
# record in the job log. Pin both by digest so the privileged + build surface
# is reproducible and auditable, and a future upstream regression can't
# silently change every gocdnext build at once. Operators override the builder
# via PLUGIN_BUILDKIT_IMAGE; binfmt is internal. Bump = new digest here.
BINFMT_IMAGE="tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"
DEFAULT_BUILDKIT_IMAGE="moby/buildkit:v0.34.2@sha256:c5f8862f52499773af85bc07695b2379ef08e8002352b0df2c4864c91d78dabe"

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

# Layer compression codec (optional). Validated by whitelist up front so a
# typo fails fast — before the 60s daemon wait + buildkit boot — and can never
# be interpolated into the --output arg as anything but a known token.
COMPRESSION="$(trim "${PLUGIN_COMPRESSION:-}")"
case "${COMPRESSION}" in
    ""|gzip|zstd|uncompressed|estargz) ;;
    *)
        echo "gocdnext/buildx: unknown compression '${COMPRESSION}' — use gzip, zstd, uncompressed, or estargz" >&2
        exit 2
        ;;
esac

# S3 path-style addressing for the `cache: bucket` backend (opt-in, #335).
# MinIO and other S3-compatible stores that don't publish bucket.<endpoint>
# virtual-host DNS need path-style requests; AWS S3, GCS interop, and R2 work
# with virtual-host and must stay as-is. BuildKit's s3 cache exposes this choice
# explicitly and defaults to false, so we DON'T infer it from the endpoint (a
# "custom endpoint" doesn't determine the right style) — the operator opts in
# with GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE=true. Validated up front so a typo
# fails here, before the 60s daemon wait and the builder boot.
USE_PATH_STYLE="$(trim "${GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE:-}")"
case "${USE_PATH_STYLE}" in
    ""|false|true) ;;
    *)
        echo "gocdnext/buildx: GOCDNEXT_LAYER_CACHE_USE_PATH_STYLE must be 'true' or 'false' (got '${USE_PATH_STYLE}')" >&2
        exit 2
        ;;
esac

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
    echo "==> registering QEMU handlers (binfmt ${BINFMT_IMAGE##*@}) — cross-arch target"
    docker run --privileged --rm "${BINFMT_IMAGE}" --install all >/dev/null
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
# Set only on the bucket/remote path below; the cleanup trap keys off them.
BUILDKIT_CONTAINER=""
STAGE_DIR=""
BUILDKITD_CONFIG=""
# Trap installed BEFORE any builder/container/file is created, so a failure at
# ANY step still tears everything down. Order: buildx builder metadata → the
# self-managed BuildKit container → temp files (the staged credentials last).
# Idempotent — every step is guarded + `|| true`, so a partial setup (e.g. the
# container was created but never started, or creation failed outright) cleans
# up without error.
cleanup() {
    docker buildx rm "${BUILDER}" >/dev/null 2>&1 || true
    [ -n "${BUILDKIT_CONTAINER}" ] && docker rm -f "${BUILDKIT_CONTAINER}" >/dev/null 2>&1 || true
    [ -n "${STAGE_DIR}" ] && rm -rf "${STAGE_DIR}" 2>/dev/null || true
    [ -n "${BUILDKITD_CONFIG}" ] && rm -f "${BUILDKITD_CONFIG}" 2>/dev/null || true
    return 0
}
trap cleanup EXIT

# Builder image: operator override wins as-is; otherwise the pinned default
# (top of file). Never empty, so neither builder path falls back to buildx's
# floating buildx-stable-1.
buildkit_image="$(trim "${PLUGIN_BUILDKIT_IMAGE:-}")"
if [ -z "${buildkit_image}" ]; then
    buildkit_image="${DEFAULT_BUILDKIT_IMAGE}"
fi

# Registry mirror for docker.io base-image pulls (issue #35). Without it every
# FROM resolves straight to Docker Hub from the builder — anonymous pulls are
# rate-limited per IP and clusters egress through a handful of NAT IPs, so a
# cold-cache rebuild day can exhaust the quota. The mirror lands in a
# buildkitd.toml the builder reads; BuildKit falls back to the upstream registry
# automatically when the mirror misses or is down. Both builder paths below
# consume BUILDKITD_CONFIG (docker-container: `--config`; remote: `docker cp`).
PLUGIN_MIRROR="$(trim "${PLUGIN_MIRROR:-}")"
if [ -n "${PLUGIN_MIRROR}" ]; then
    mirror="${PLUGIN_MIRROR#https://}"
    mirror="${mirror#http://}"
    mirror="${mirror%/}"
    # The value is interpolated into TOML — reject anything outside
    # host[:port][/path] characters so a stray quote can't smuggle
    # config keys into the builder (defence in depth; with: inputs
    # are operator-authored, not untrusted).
    if [[ ! "${mirror}" =~ ^[A-Za-z0-9._:/-]+$ ]]; then
        echo "gocdnext/buildx: mirror '${PLUGIN_MIRROR}' has characters outside [A-Za-z0-9._:/-]" >&2
        exit 2
    fi
    BUILDKITD_CONFIG="$(mktemp)"
    cat > "${BUILDKITD_CONFIG}" <<EOF
[registry."docker.io"]
  mirrors = ["${mirror}"]
EOF
    echo "==> docker.io pulls mirrored via ${mirror} (fallback: upstream)"
    # Authenticated mirrors: the login happens in THIS container — buildx
    # forwards registry auth to BuildKit through the client session's auth
    # provider (remote builder included), mirrors among them. Credentials ride
    # the job's `secrets:`, same masking contract as the push creds.
    if [ -n "${PLUGIN_MIRROR_USERNAME:-}" ]; then
        mirror_host="${mirror%%/*}"
        echo "==> logging into mirror ${mirror_host} as ${PLUGIN_MIRROR_USERNAME}"
        echo "${PLUGIN_MIRROR_PASSWORD:-}" | docker login "${mirror_host}" \
            --username "${PLUGIN_MIRROR_USERNAME}" --password-stdin
    fi
fi

if [ "${PLUGIN_CACHE:-}" = "bucket" ]; then
    # --- cache: bucket → self-managed BuildKit + remote driver (#335) --------
    # The docker-container driver's ONLY way to put env in the builder is
    # `--driver-opt env.NAME=VALUE`, which lands VALUE on `docker buildx
    # create`'s argv (visible via `ps` / /proc/<pid>/cmdline / any sidecar in
    # the PID namespace). For the S3 cache backend that value is
    # AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN. Even `docker run -e NAME`
    # (value read from our env, off argv) would still store the resolved value
    # in the container's Config.Env, readable via `docker inspect`. So we run
    # BuildKit ourselves and hand it the secrets through an AWS shared
    # credentials FILE copied in with `docker cp` — the value crosses no argv
    # or Config.Env boundary — then attach buildx via the remote driver's
    # docker-container:// endpoint. Only non-secret knobs (the checksum opt-out,
    # the credentials-file PATH) ride as env.
    #
    # Residual, by construction: whoever controls docker.sock controls the
    # daemon and can read the builder's filesystem. This eliminates incidental
    # argv/metadata exposure, NOT the Docker admin.
    BUILDKIT_CONTAINER="gocdnext-buildkitd-${RANDOM}"
    STAGE_DIR="$(mktemp -d)"
    mkdir "${STAGE_DIR}/gocdnext"
    staged=0

    bk_create_args=(docker create --rm --privileged --name "${BUILDKIT_CONTAINER}")
    if [ "${needs_no_checksum}" = "1" ]; then
        # Non-AWS S3 endpoint (GCS/MinIO/R2): recent aws-sdk-go-v2 sends
        # x-amz-checksum-* headers on PutObject by default; those endpoints
        # fold them into the v4 signature and reject the call as
        # SignatureDoesNotMatch (surfaces as a generic 403). These two env vars
        # opt out. Non-secret values — fine on argv / in Config.Env.
        echo "==> buildkit: disabling AWS SDK auto-checksum (non-AWS S3 endpoint detected)"
        bk_create_args+=(-e "AWS_REQUEST_CHECKSUM_CALCULATION=when_required")
        bk_create_args+=(-e "AWS_RESPONSE_CHECKSUM_VALIDATION=when_required")
    fi
    # AWS credentials → an AWS shared-credentials file staged with `docker cp`.
    # BuildKit's S3 client reads it via the AWS Go SDK default chain, pointed at
    # it by AWS_SHARED_CREDENTIALS_FILE (a non-secret PATH, so it may ride env).
    # Written umask 077 so the secret is 0600 before it ever leaves this
    # container. Only when static keys are present — the gcs/s3 cache specs
    # already require them; an env-less builder would fall back to its own chain
    # (IMDS/IRSA), which is a separate follow-up.
    if [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
        ( umask 077
          {
            printf '[default]\n'
            printf 'aws_access_key_id = %s\n' "${AWS_ACCESS_KEY_ID}"
            printf 'aws_secret_access_key = %s\n' "${AWS_SECRET_ACCESS_KEY}"
            # if-block, NOT `[ -n … ] && printf`: the latter is the group's last
            # command, so when there's no session token (the common static-key
            # case) it returns 1, the subshell exits 1, and `set -e` would kill
            # the plugin. The if returns 0 whether or not the token is present.
            if [ -n "${AWS_SESSION_TOKEN:-}" ]; then
                printf 'aws_session_token = %s\n' "${AWS_SESSION_TOKEN}"
            fi
          } > "${STAGE_DIR}/gocdnext/aws-credentials" )
        bk_create_args+=(-e "AWS_SHARED_CREDENTIALS_FILE=/etc/gocdnext/aws-credentials")
        staged=1
        echo "==> buildkit: AWS credentials staged as a shared-credentials file (not argv, not Config.Env)"
    fi
    bk_create_args+=("${buildkit_image}")
    # buildkitd flags appended AFTER the image (the moby/buildkit entrypoint is
    # buildkitd). Point it at the mirror config staged below when present; the
    # default listen socket is kept so the remote docker-container:// endpoint
    # can dial it.
    if [ -n "${BUILDKITD_CONFIG}" ]; then
        cp "${BUILDKITD_CONFIG}" "${STAGE_DIR}/gocdnext/buildkitd.toml"
        bk_create_args+=(--config /etc/gocdnext/buildkitd.toml)
        staged=1
    fi

    "${bk_create_args[@]}" >/dev/null
    # Stage files BEFORE start (buildkitd reads its config, and the SDK reads
    # the creds, at runtime). `docker cp` of the directory creates /etc/gocdnext
    # inside the container and preserves the 0600 mode — and, unlike a bind
    # mount, doesn't depend on the DinD host filesystem.
    if [ "${staged}" = "1" ]; then
        docker cp "${STAGE_DIR}/gocdnext" "${BUILDKIT_CONTAINER}:/etc/" >/dev/null
    fi
    docker start "${BUILDKIT_CONTAINER}" >/dev/null

    # The remote driver dials buildkitd over the container's socket; give it a
    # moment to come up (a cold buildkitd listens in ~1-2s). Probe with buildctl
    # inside the container rather than racing `buildx create`.
    echo "==> waiting for self-managed BuildKit (${BUILDKIT_CONTAINER})"
    for i in $(seq 1 30); do
        if docker exec "${BUILDKIT_CONTAINER}" buildctl debug workers >/dev/null 2>&1; then
            break
        fi
        if [ "${i}" = "30" ]; then
            echo "gocdnext/buildx: self-managed BuildKit did not become ready within 30s" >&2
            docker logs "${BUILDKIT_CONTAINER}" 2>&1 | tail -20 >&2 || true
            exit 2
        fi
        sleep 1
    done

    docker buildx create --name "${BUILDER}" --use \
        --driver remote "docker-container://${BUILDKIT_CONTAINER}" >/dev/null
    docker buildx inspect --bootstrap >/dev/null
    echo "==> using self-managed BuildKit via remote driver (AWS creds off argv/Config.Env)"
else
    # --- registry / inline / none → the standard docker-container driver -----
    # No AWS secret is needed for these cache modes, so the argv concern that
    # drives the bucket path above doesn't apply; buildx manages the builder
    # container lifecycle itself.
    buildx_create_args=(--name "${BUILDER}" --use --driver-opt "image=${buildkit_image}")
    if [ -n "${BUILDKITD_CONFIG}" ]; then
        buildx_create_args+=(--config "${BUILDKITD_CONFIG}")
    fi
    docker buildx create "${buildx_create_args[@]}" >/dev/null
    docker buildx inspect --bootstrap >/dev/null
fi

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
#
# `COMPRESSION` (validated up front) sets the layer codec. `--push` is just
# shorthand for `--output type=image,push=true`, so to pick a codec we expand
# it to the explicit output form and append compression=. zstd/uncompressed
# kill the single-thread gzip export cost on large incompressible layers. Only
# applied when pushing — a build-only run has no image export to compress. The
# `-t` tags still supply the image name(s); we don't put name= in the output.
push_args=()
if [ "${PUSH}" = "true" ]; then
    if [ -n "${COMPRESSION}" ] && [ "${COMPRESSION}" != "gzip" ]; then
        push_args+=("--output" "type=image,compression=${COMPRESSION},push=true")
        echo "==> layer compression: ${COMPRESSION}"
    else
        push_args+=("--push")
    fi
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
            # Opt-in path-style addressing (validated up front). Appended to the
            # shared spec so it lands on BOTH cache-to and cache-from — a
            # half-applied style silently breaks the import side.
            if [ "${USE_PATH_STYLE}" = "true" ]; then
                spec="${spec},use_path_style=true"
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
