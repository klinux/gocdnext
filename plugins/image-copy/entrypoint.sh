#!/bin/bash
# gocdnext/image-copy — promote a container image (multi-arch
# manifest preserved) from SOURCE registry/tag to TARGET. See
# Dockerfile for the full contract.

set -euo pipefail

SOURCE="${PLUGIN_SOURCE:-}"
TARGET="${PLUGIN_TARGET:-}"
EXTRA_TAGS_RAW="${PLUGIN_EXTRA_TAGS:-}"
BACKEND="${PLUGIN_BACKEND:-crane}"
OUTPUT="${PLUGIN_OUTPUT:-.gocdnext/image-copy.env}"

TGT_USER="${PLUGIN_USERNAME:-}"
TGT_PASS="${PLUGIN_PASSWORD:-}"
SRC_USER="${PLUGIN_SOURCE_USERNAME:-}"
SRC_PASS="${PLUGIN_SOURCE_PASSWORD:-}"

# OCI tag charset (per the OCI distribution spec):
# starts with [A-Za-z0-9_]; rest is [A-Za-z0-9_.-]{0,127}. No `+`
# despite some registries accepting it — strict so tags travel
# cleanly across all conformant registries. Applied to both the
# primary tag in TARGET and every entry of EXTRA_TAGS.
tag_charset='^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$'

# --- validate inputs --------------------------------------------------

if [ -z "${SOURCE}" ]; then
    echo "gocdnext/image-copy: PLUGIN_SOURCE is required" >&2
    echo "  example: source: ghcr.io/org/app:staging-1234" >&2
    exit 2
fi
if [ -z "${TARGET}" ]; then
    echo "gocdnext/image-copy: PLUGIN_TARGET is required" >&2
    echo "  example: target: ghcr.io/org/app:v1.2.3" >&2
    exit 2
fi

# Image refs land on the backend command line. Restrict to a
# permissive but bounded charset: alphanumerics, dot, hyphen, slash,
# underscore, colon (tag separator), at-sign (digest separator),
# plus sha256 hex. Reject shell metas, spaces, quotes — same
# defence-in-depth as semver-bump's PREFIX guard.
ref_charset='^[A-Za-z0-9._/:@-]+$'
if ! [[ "${SOURCE}" =~ ${ref_charset} ]]; then
    echo "gocdnext/image-copy: PLUGIN_SOURCE contains forbidden characters — got: ${SOURCE}" >&2
    echo "  accepted charset: [A-Za-z0-9._/:@-]+" >&2
    exit 2
fi
if ! [[ "${TARGET}" =~ ${ref_charset} ]]; then
    echo "gocdnext/image-copy: PLUGIN_TARGET contains forbidden characters — got: ${TARGET}" >&2
    exit 2
fi

# Output path: workspace-relative, no absolute, no `..` traversal.
# Matches the semver-bump guard exactly.
if [ -z "${OUTPUT}" ]; then
    echo "gocdnext/image-copy: PLUGIN_OUTPUT must not be empty" >&2
    exit 2
fi
case "${OUTPUT}" in
    /*)
        echo "gocdnext/image-copy: PLUGIN_OUTPUT must be a workspace-relative path (no leading /) — got: ${OUTPUT}" >&2
        exit 2
        ;;
esac
case "${OUTPUT}" in
    ../*|*/../*|*/..)
        echo "gocdnext/image-copy: PLUGIN_OUTPUT must not traverse outside the workspace — got: ${OUTPUT}" >&2
        exit 2
        ;;
esac

case "${BACKEND}" in
    crane|skopeo|buildx-imagetools) ;;
    *)
        echo "gocdnext/image-copy: PLUGIN_BACKEND must be one of: crane, skopeo, buildx-imagetools — got: ${BACKEND}" >&2
        exit 2
        ;;
esac

# Parse extra-tags: newline-separated, trim whitespace, validate
# each tag matches a conservative tag charset (OCI tag spec is
# `[A-Za-z0-9_.-]+`, max 128 chars — we go slightly broader to
# allow `+` since some registries accept it).
# OCI tag spec: must START with alphanumeric or underscore (NOT
# `.` or `-` so a tag can't be confused with a path/hidden file),
# then 0–127 of [A-Za-z0-9_.-]. No `+` despite some registries
# accepting it — we stay strict to the spec so tags travel
# cleanly across all conformant registries. tag_charset was
# defined above for the primary target tag; reused here.
EXTRA_TAGS=()
if [ -n "${EXTRA_TAGS_RAW}" ]; then
    while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}" # ltrim
        line="${line%"${line##*[![:space:]]}"}" # rtrim
        [ -z "${line}" ] && continue
        if ! [[ "${line}" =~ ${tag_charset} ]]; then
            echo "gocdnext/image-copy: PLUGIN_EXTRA_TAGS entry rejected (bad OCI tag) — got: ${line}" >&2
            echo "  accepted: starts with [A-Za-z0-9_], then [A-Za-z0-9_.-]{0,127}" >&2
            exit 2
        fi
        EXTRA_TAGS+=("${line}")
    done <<< "${EXTRA_TAGS_RAW}"
fi

# --- resolve hosts + finalise creds before authfile ------------------

# Source/target host = first path segment. We need this BEFORE
# defaulting source creds, so we can gate the default on same-host.
source_host="${SOURCE%%/*}"
target_host="${TARGET%%/*}"

# A "registry host" must look like a host — contain a `.`, contain
# a `:` (host:port), OR be literal `localhost`. Otherwise treat it
# as a Docker Hub shorthand (`org/app:tag`) and bail loud — image-
# copy is supposed to be precise across registries; a silent
# default-to-docker.io is exactly the kind of cross-registry
# confusion this plugin is meant to AVOID.
is_real_host() {
    local h="$1"
    [[ "${h}" == *"."* ]] && return 0
    [[ "${h}" == *":"* ]] && return 0
    [[ "${h}" == "localhost" ]] && return 0
    return 1
}
if ! [[ "${SOURCE}" == *"/"* ]] || ! is_real_host "${source_host}"; then
    echo "gocdnext/image-copy: PLUGIN_SOURCE must include a real registry host (e.g. ghcr.io/org/app:tag, not org/app:tag) — got: ${SOURCE}" >&2
    exit 2
fi
if ! [[ "${TARGET}" == *"/"* ]] || ! is_real_host "${target_host}"; then
    echo "gocdnext/image-copy: PLUGIN_TARGET must include a real registry host — got: ${TARGET}" >&2
    exit 2
fi

# TARGET must be a tag-form ref (registry/repo:tag), NOT a digest
# pin and NOT bare-repo. Promotion semantics require a tag to
# write to; a digest pin would mean "create a tag whose name is
# the digest", which the registry semantics don't permit anyway.
if [[ "${TARGET}" == *"@"* ]]; then
    echo "gocdnext/image-copy: PLUGIN_TARGET must be tag form (no @digest) — got: ${TARGET}" >&2
    echo "  the source can be digest-pinned; the target must be a tag the registry can write." >&2
    exit 2
fi
# Tag form check: there must be a `:` AFTER the last `/`, AND the
# bit after the `:` must satisfy the OCI tag spec — same regex
# extra-tags is held to. Otherwise the registry would refuse the
# write later (`ghcr.io/x/y:` empty tag, `ghcr.io/x/y:-bad`
# leading-dash) but with a much more confusing error.
target_after_last_slash="${TARGET##*/}"
if ! [[ "${target_after_last_slash}" == *":"* ]]; then
    echo "gocdnext/image-copy: PLUGIN_TARGET must include a tag (registry/repo:tag) — got: ${TARGET}" >&2
    exit 2
fi
target_primary_tag="${target_after_last_slash#*:}"
if ! [[ "${target_primary_tag}" =~ ${tag_charset} ]]; then
    echo "gocdnext/image-copy: PLUGIN_TARGET tag fails the OCI tag spec — got tag: ${target_primary_tag}" >&2
    echo "  accepted: starts with [A-Za-z0-9_], then [A-Za-z0-9_.-]{0,127}" >&2
    exit 2
fi

# Default source creds to target creds ONLY when source + target
# share the host. Cross-registry without explicit source creds
# leaves the source anonymous (legitimate for public images) — we
# do NOT cross-pollute the target token into a stranger's authfile
# entry.
if [ "${source_host}" = "${target_host}" ]; then
    if [ -z "${SRC_USER}" ] && [ -n "${TGT_USER}" ]; then
        SRC_USER="${TGT_USER}"
    fi
    if [ -z "${SRC_PASS}" ] && [ -n "${TGT_PASS}" ]; then
        SRC_PASS="${TGT_PASS}"
    fi
fi

# --- build shared authfile (docker config.json format) ----------------

# crane and skopeo both honour the docker config.json auth format.
# buildx-imagetools uses the docker daemon's view of auth (set via
# `docker login`), so we run those separately in that branch below.
#
# Build the JSON in a private tempdir. Variables are set at SCRIPT
# scope (not in a function called via $()) so the EXIT trap can
# actually clean up — a subshell function would leave auth_dir
# orphan in the parent's scope, breaking both DOCKER_CONFIG export
# AND the cleanup. The path: `auth_dir` (global) → trap cleanup_auth
# reads it on every exit path.
auth_dir=""
cleanup_auth() {
    if [ -n "${auth_dir:-}" ] && [ -d "${auth_dir}" ]; then
        rm -rf "${auth_dir}"
    fi
}
trap cleanup_auth EXIT INT TERM

auth_dir=$(mktemp -d)
chmod 700 "${auth_dir}"
authfile="${auth_dir}/config.json"

# Build the auths map. base64 -w0 — single line, no wrapping.
# coreutils provides it (BusyBox base64 silently wraps at 76 chars
# and breaks the docker config.json string value).
auth_entries=""
if [ -n "${TGT_USER}" ] && [ -n "${TGT_PASS}" ]; then
    tgt_b64=$(printf '%s:%s' "${TGT_USER}" "${TGT_PASS}" | base64 -w0)
    auth_entries=$(jq -n --arg host "${target_host}" --arg auth "${tgt_b64}" \
        '{($host): {auth: $auth}}')
fi
if [ -n "${SRC_USER}" ] && [ -n "${SRC_PASS}" ] && [ "${source_host}" != "${target_host}" ]; then
    src_b64=$(printf '%s:%s' "${SRC_USER}" "${SRC_PASS}" | base64 -w0)
    if [ -z "${auth_entries}" ]; then
        auth_entries=$(jq -n --arg host "${source_host}" --arg auth "${src_b64}" \
            '{($host): {auth: $auth}}')
    else
        auth_entries=$(echo "${auth_entries}" | jq --arg host "${source_host}" --arg auth "${src_b64}" \
            '. + {($host): {auth: $auth}}')
    fi
fi

if [ -z "${auth_entries}" ]; then
    echo '{"auths": {}}' > "${authfile}"
else
    echo "${auth_entries}" | jq '{auths: .}' > "${authfile}"
fi
chmod 600 "${authfile}"

# --- execute copy via chosen backend ----------------------------------

echo "==> image-copy: ${SOURCE} → ${TARGET} via ${BACKEND}"

case "${BACKEND}" in
    crane)
        export DOCKER_CONFIG="${auth_dir}"
        # `crane copy` preserves the manifest list for multi-arch
        # sources — that's the whole point of using it over
        # `docker pull|tag|push`. Failure surfaces via crane's
        # non-zero exit; we let set -e propagate.
        crane copy "${SOURCE}" "${TARGET}"
        for extra in "${EXTRA_TAGS[@]}"; do
            echo "==> retag (crane): ${TARGET} → ${extra}"
            # `crane tag SRC NEWTAG` requires the second arg to be
            # just the tag name (it stays in the same repo as SRC).
            crane tag "${TARGET}" "${extra}"
        done
        unset DOCKER_CONFIG
        ;;
    skopeo)
        # --multi-arch all: preserve the FULL manifest list. Without
        # it skopeo would resolve to the current platform's manifest
        # and break consumers on other architectures.
        #
        # NOTE: this does NOT copy cosign signatures or attestations
        # — those live as SEPARATE registry artifacts (`sha256-X.sig`
        # tags discovered via the cosign triangulation or the OCI
        # `referrers` API). For signed-image promotion the proper
        # tool is `cosign copy SRC DST` which knows about the
        # triangulation; until image-copy ships a `cosign-copy`
        # backend, re-sign at the target instead.
        skopeo copy --authfile "${authfile}" \
            --multi-arch all \
            "docker://${SOURCE}" "docker://${TARGET}"
        for extra in "${EXTRA_TAGS[@]}"; do
            target_repo="${TARGET%:*}"
            echo "==> retag (skopeo): ${TARGET} → ${target_repo}:${extra}"
            skopeo copy --authfile "${authfile}" \
                --multi-arch all \
                "docker://${TARGET}" "docker://${target_repo}:${extra}"
        done
        ;;
    buildx-imagetools)
        # buildx uses docker's own auth mechanism rather than an
        # authfile we hand-craft. We point DOCKER_CONFIG at our
        # trap-cleaned tempdir BEFORE `docker login` so the
        # resulting credential lands there, not in
        # $HOME/.docker/config.json (which would persist on the
        # job filesystem after the job exits and survive even a
        # clean run). Login uses --password-stdin so the password
        # never lands on argv; printf '%s\n' (rather than echo)
        # avoids shells that interpret backslash escapes in echo
        # by default.
        if ! command -v docker >/dev/null 2>&1; then
            echo "gocdnext/image-copy: backend=buildx-imagetools needs the docker CLI in the job container" >&2
            echo "  set 'docker: true' on the job so the agent exposes the docker socket" >&2
            exit 3
        fi
        export DOCKER_CONFIG="${auth_dir}"
        if [ -n "${TGT_USER}" ] && [ -n "${TGT_PASS}" ]; then
            echo "==> docker login ${target_host} as ${TGT_USER}"
            printf '%s\n' "${TGT_PASS}" | docker login "${target_host}" \
                --username "${TGT_USER}" --password-stdin
        fi
        if [ -n "${SRC_USER}" ] && [ -n "${SRC_PASS}" ] && [ "${source_host}" != "${target_host}" ]; then
            echo "==> docker login ${source_host} as ${SRC_USER}"
            printf '%s\n' "${SRC_PASS}" | docker login "${source_host}" \
                --username "${SRC_USER}" --password-stdin
        fi
        # buildx imagetools create assembles the target manifest by
        # referencing the source manifest list — natively multi-arch.
        # All extra tags can ride a single invocation.
        all_tags=("--tag" "${TARGET}")
        target_repo="${TARGET%:*}"
        for extra in "${EXTRA_TAGS[@]}"; do
            all_tags+=("--tag" "${target_repo}:${extra}")
        done
        docker buildx imagetools create "${all_tags[@]}" "${SOURCE}"
        unset DOCKER_CONFIG
        ;;
esac

# --- resolve promoted digest ------------------------------------------

# Always use crane to resolve the final digest — it's the smallest +
# most predictable digest tool of the three and we already ship it.
# For multi-arch images this returns the digest of the manifest LIST
# (the index), which is what a downstream cosign-sign wants to anchor
# at. For single-arch it's the manifest digest. Both are stable.
#
# auth_dir was populated by every backend during the copy step:
# crane / skopeo branches built it manually with jq+base64; the
# buildx branch ran `docker login` against the same DOCKER_CONFIG.
# So crane digest reads the same auths regardless of backend, no
# special-casing needed here.
export DOCKER_CONFIG="${auth_dir}"
promoted_digest=$(crane digest "${TARGET}" 2>/dev/null || true)
unset DOCKER_CONFIG

# Treat missing digest as a hard failure — PROMOTED_DIGEST is the
# CENTRAL output of this plugin (downstream cosign sign-by-digest,
# audit trails, anchored deployments all depend on it). Emitting
# PROMOTED_DIGEST='' silently would push the failure to whichever
# consumer touches it first, with a confusing error far from the
# root cause. Fail here instead — the copy DID succeed, so the
# operator can retry the digest resolution (registry transient,
# permissions, network) without re-running the upload.
if [ -z "${promoted_digest}" ]; then
    echo "gocdnext/image-copy: failed to resolve digest for ${TARGET}" >&2
    echo "  the copy itself succeeded; the digest read against the target failed." >&2
    echo "  check: registry read permissions, eventual-consistency window, network reach." >&2
    exit 3
fi

# --- write output files -----------------------------------------------

# Path 1 (legacy, kept for backward compat): the workspace file
# downstream consumers `source` after `needs_artifacts:`.
output_dir=$(dirname "${OUTPUT}")
if [ "${output_dir}" != "." ]; then
    mkdir -p "${output_dir}"
fi
{
    echo "# Generated by gocdnext/image-copy — do not edit."
    echo "PROMOTED_DIGEST='${promoted_digest}'"
    echo "SOURCE='${SOURCE}'"
    echo "TARGET='${TARGET}'"
    echo "BACKEND='${BACKEND}'"
} > "${OUTPUT}"

# Path 2 (native, since gocdnext v0.11.0): GOCDNEXT_OUTPUT_FILE
# is the agent-managed path the scheduler reads after the job
# ends. Downstream jobs reference values via
# `${{ needs.<this-job>.outputs.<alias> }}` resolved at dispatch
# (no `needs_artifacts:` + `source` step required). The agent
# only ships keys the operator DECLARED in their `outputs:`
# block — extras are silently dropped, so writing all four here
# costs nothing and lets the operator declare a subset:
#
#   outputs:
#     digest: PROMOTED_DIGEST   # most common — anchors cosign sign
#
# When GOCDNEXT_OUTPUT_FILE is empty (older agents, or the job
# didn't declare any `outputs:`), the writes silently no-op.
if [ -n "${GOCDNEXT_OUTPUT_FILE:-}" ]; then
    {
        echo "PROMOTED_DIGEST=${promoted_digest}"
        echo "SOURCE=${SOURCE}"
        echo "TARGET=${TARGET}"
        echo "BACKEND=${BACKEND}"
    } > "${GOCDNEXT_OUTPUT_FILE}"
fi

# --- echo to job log --------------------------------------------------

echo "==> image-copy: done"
echo "      SOURCE          = ${SOURCE}"
echo "      TARGET          = ${TARGET}"
if [ -n "${promoted_digest}" ]; then
    echo "      PROMOTED_DIGEST = ${promoted_digest}"
fi
if [ ${#EXTRA_TAGS[@]} -gt 0 ]; then
    echo "      EXTRA_TAGS      = ${EXTRA_TAGS[*]}"
fi
echo "      BACKEND         = ${BACKEND}"
echo "      written to: ${OUTPUT}"
