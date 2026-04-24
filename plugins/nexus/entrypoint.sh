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

# resolve_latest hits Nexus's asset search endpoint, sorted
# newest-first, and returns the downloadUrl of the first result
# matching the Maven coordinates. Nexus's "sort=version" uses
# its own comparator that handles semver + snapshot suffixes
# correctly — more reliable than shelling out a semver sort.
resolve_latest() {
    local query
    query="repository=$(urlencode "${repo}")"
    query="${query}&group=$(urlencode "${group}")"
    query="${query}&name=$(urlencode "${artifact}")"
    query="${query}&maven.extension=$(urlencode "${ext}")"
    if [ -n "${classifier}" ]; then
        query="${query}&maven.classifier=$(urlencode "${classifier}")"
    fi
    query="${query}&sort=version&direction=desc"

    # /search/assets returns one row per matching asset, newest
    # first; pick .items[0].downloadUrl.
    local url="${base}/service/rest/v1/search/assets?${query}"
    local body
    body=$(curl -fSsL "${auth_args[@]}" "${url}")
    local download_url
    download_url=$(printf '%s' "${body}" | jq -r '.items[0].downloadUrl // empty')
    if [ -z "${download_url}" ]; then
        echo "gocdnext/nexus: no asset matched ${group}:${artifact}:${ext}${classifier:+:${classifier}}" >&2
        exit 2
    fi
    printf '%s' "${download_url}"
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

urlencode() {
    # jq's @uri covers the standard percent-encoding set. Avoids
    # a perl dep for a one-liner.
    jq -rn --arg v "$1" '$v|@uri'
}

if [ "${PLUGIN_ACTION}" = "download" ]; then
    version="${PLUGIN_VERSION:-}"
    if [ -z "${version}" ]; then
        echo "gocdnext/nexus: version is required for action=download (or 'LATEST')" >&2
        exit 2
    fi
    dest_rel="${PLUGIN_DEST:-.}"
    dest="/workspace/${dest_rel#/}"
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
    src="/workspace/${file_rel#/}"
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
