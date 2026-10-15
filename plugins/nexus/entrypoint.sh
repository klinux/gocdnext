#!/bin/bash
# gocdnext/nexus — upload/download via Nexus REST API. See
# Dockerfile for the full contract.

set -euo pipefail

for var in PLUGIN_ACTION PLUGIN_URL PLUGIN_REPOSITORY PLUGIN_GROUP_ID PLUGIN_ARTIFACT_ID; do
    if [ -z "${!var:-}" ]; then
        echo "gocdnext/nexus: ${var,,} is required" >&2
        exit 2
    fi
done

base="${PLUGIN_URL%/}"
repo="${PLUGIN_REPOSITORY}"
group="${PLUGIN_GROUP_ID}"
artifact="${PLUGIN_ARTIFACT_ID}"
ext="${PLUGIN_EXTENSION:-jar}"
classifier="${PLUGIN_CLASSIFIER:-}"

auth_args=()
if [ -n "${PLUGIN_USERNAME:-}" ]; then
    # curl's --user flag ships credentials as Authorization:
    # Basic <b64>; same as putting them in a ~/.netrc but stays
    # entirely in memory. PASSWORD is allowed empty (Nexus tokens
    # sometimes pair with a blank password) so we don't reject.
    auth_args=(--user "${PLUGIN_USERNAME}:${PLUGIN_PASSWORD:-}")
fi

case "${PLUGIN_ACTION}" in
    download) ;;
    upload) ;;
    *)
        echo "gocdnext/nexus: unknown action '${PLUGIN_ACTION}' (accepted: upload, download)" >&2
        exit 2
        ;;
esac

# resolve_latest finds the newest version from the repository's
# maven-metadata.xml under /repository/ — the SAME endpoint a plain
# Maven/GoCD fetch uses — and returns the direct /repository/ download URL
# for it.
#
# Why not the /service/rest search API: that endpoint needs the `nx-search`
# privilege, which a Maven-scoped CI/deploy user (repo-read only) typically
# does NOT have — so it 401s even when the very same credentials download
# fine from /repository/. maven-metadata.xml needs only repo-read, so LATEST
# now works with the same role that already pulls pinned versions.
#
# <release> is the newest non-snapshot release; fall back to <latest> (which
# may include a snapshot) and then to the last-listed <version> (metadata
# lists versions oldest→newest). SNAPSHOT builds would need the per-version
# metadata to resolve the timestamped filename — out of scope here, same as
# the old search path in practice; pin an explicit version for those.
resolve_latest() {
    local group_path="${group//.//}"
    local meta_url="${base}/repository/${repo}/${group_path}/${artifact}/maven-metadata.xml"
    local meta
    meta=$(curl -fSsL "${auth_args[@]}" "${meta_url}")

    local version
    version=$(printf '%s' "${meta}" | grep -oE '<release>[^<]+</release>' | head -1 | sed -E 's/<[^>]+>//g')
    if [ -z "${version}" ]; then
        version=$(printf '%s' "${meta}" | grep -oE '<latest>[^<]+</latest>' | head -1 | sed -E 's/<[^>]+>//g')
    fi
    if [ -z "${version}" ]; then
        version=$(printf '%s' "${meta}" | grep -oE '<version>[^<]+</version>' | tail -1 | sed -E 's/<[^>]+>//g')
    fi
    if [ -z "${version}" ]; then
        echo "gocdnext/nexus: could not resolve LATEST for ${group}:${artifact} from ${meta_url}" >&2
        exit 2
    fi

    printf '%s/repository/%s/%s' "${base}" "${repo}" "$(maven_path "${version}")"
}

# maven_path builds the repository-relative path for a specific
# (group, artifact, version, ext[, classifier]) tuple using the
# Maven 2 layout: <group>/<artifact>/<version>/<artifact>-<version>[-<classifier>].<ext>
maven_path() {
    local version="$1"
    local group_path="${group//./\/}"
    local fname="${artifact}-${version}"
    if [ -n "${classifier}" ]; then
        fname="${fname}-${classifier}"
    fi
    fname="${fname}.${ext}"
    printf '%s/%s/%s/%s' "${group_path}" "${artifact}" "${version}" "${fname}"
}

if [ "${PLUGIN_ACTION}" = "download" ]; then
    version="${PLUGIN_VERSION:-}"
    if [ -z "${version}" ]; then
        echo "gocdnext/nexus: version is required for action=download (or 'LATEST')" >&2
        exit 2
    fi
    dest_rel="${PLUGIN_DEST:-.}"
    dest="${dest_rel#/}"
    mkdir -p "$(dirname "${dest}/placeholder")"
    if [ "${version}" = "LATEST" ]; then
        url=$(resolve_latest)
    else
        url="${base}/repository/${repo}/$(maven_path "${version}")"
    fi
    # If dest is a directory, derive the filename from the URL
    # path. If it's a file path, save with that name exactly —
    # operators who want to rename ("shared-lib-LATEST.jar")
    # pass a full path.
    if [ -d "${dest}" ]; then
        fname="${url##*/}"
        dest="${dest%/}/${fname}"
    fi
    echo "==> GET ${url} -> ${dest}"
    curl -fSsL "${auth_args[@]}" -o "${dest}" "${url}"
    echo "==> downloaded $(stat -c%s "${dest}" 2>/dev/null || echo "?") bytes"
fi

if [ "${PLUGIN_ACTION}" = "upload" ]; then
    file_rel="${PLUGIN_FILE:-}"
    version="${PLUGIN_UPLOAD_VERSION:-}"
    if [ -z "${file_rel}" ] || [ -z "${version}" ]; then
        echo "gocdnext/nexus: file + upload-version are required for action=upload" >&2
        exit 2
    fi
    src="${file_rel#/}"
    if [ ! -f "${src}" ]; then
        echo "gocdnext/nexus: ${file_rel} not found in workspace" >&2
        exit 2
    fi
    path=$(maven_path "${version}")
    url="${base}/repository/${repo}/${path}"
    echo "==> PUT ${url} <- ${src} ($(stat -c%s "${src}" 2>/dev/null || echo "?") bytes)"
    curl -fSsL "${auth_args[@]}" \
        --upload-file "${src}" \
        "${url}"
    echo "==> uploaded"
fi
