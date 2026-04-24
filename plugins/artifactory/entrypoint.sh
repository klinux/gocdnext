#!/bin/bash
# gocdnext/artifactory — upload/download via JFrog Artifactory's
# REST API. See Dockerfile for the full contract.

set -euo pipefail

for var in PLUGIN_ACTION PLUGIN_URL PLUGIN_REPOSITORY PLUGIN_GROUP_ID PLUGIN_ARTIFACT_ID; do
    if [ -z "${!var:-}" ]; then
        echo "gocdnext/artifactory: ${var,,} is required" >&2
        exit 2
    fi
done

# Normalise the base. JFrog's cloud hosts look like
# `https://mycorp.jfrog.io` and expect `/artifactory/<repo>/...`
# underneath; self-hosted instances usually land at
# `https://myart.corp/artifactory`. Either input shape works
# here — append `/artifactory` when it's missing so the path
# math downstream stays uniform.
base="${PLUGIN_URL%/}"
case "${base}" in
    */artifactory) ;;
    *)             base="${base}/artifactory" ;;
esac

repo="${PLUGIN_REPOSITORY}"
group="${PLUGIN_GROUP_ID}"
artifact="${PLUGIN_ARTIFACT_ID}"
ext="${PLUGIN_EXTENSION:-jar}"
classifier="${PLUGIN_CLASSIFIER:-}"

# Auth: Bearer token wins when present. Falls back to basic auth
# when username is set. Either one alone is fine for public
# mirrors / anonymous reads.
auth_args=()
if [ -n "${PLUGIN_TOKEN:-}" ]; then
    auth_args=(-H "Authorization: Bearer ${PLUGIN_TOKEN}")
elif [ -n "${PLUGIN_USERNAME:-}" ]; then
    auth_args=(--user "${PLUGIN_USERNAME}:${PLUGIN_PASSWORD:-}")
fi

case "${PLUGIN_ACTION}" in
    upload|download) ;;
    *)
        echo "gocdnext/artifactory: unknown action '${PLUGIN_ACTION}' (accepted: upload, download)" >&2
        exit 2
        ;;
esac

# maven_path builds the standard Maven2 layout used by both
# actions. Same shape as the nexus plugin — Artifactory reads
# the same convention over its /artifactory/<repo>/... prefix.
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

# resolve_latest uses Artifactory Query Language (AQL) to list
# every asset matching the Maven coords, sort by created time
# descending, and take the first. AQL is Artifactory's native
# search — simpler than the older REST /search endpoints and
# supports sort criteria the Maven2-only search doesn't.
resolve_latest() {
    local group_path="${group//./\/}"
    local path_pattern="${group_path}/${artifact}/*"
    local name_pattern="${artifact}-*"
    if [ -n "${classifier}" ]; then
        name_pattern="${name_pattern}-${classifier}"
    fi
    name_pattern="${name_pattern}.${ext}"

    # AQL body: items.find with repo + path glob + name glob,
    # sort by `created` desc, limit 1. Artifactory's AQL is
    # plain text (not JSON), POSTed to /api/search/aql.
    local aql
    read -r -d '' aql <<EOF || true
items.find({
    "repo": "${repo}",
    "path": {"\$match": "${path_pattern}"},
    "name": {"\$match": "${name_pattern}"}
}).sort({"\$desc": ["created"]}).limit(1)
EOF

    local body
    body=$(curl -fSsL "${auth_args[@]}" \
        -H "Content-Type: text/plain" \
        -X POST \
        --data "${aql}" \
        "${base}/api/search/aql")
    local path name
    path=$(printf '%s' "${body}" | jq -r '.results[0].path // empty')
    name=$(printf '%s' "${body}" | jq -r '.results[0].name // empty')
    if [ -z "${path}" ] || [ -z "${name}" ]; then
        echo "gocdnext/artifactory: no asset matched ${group}:${artifact}:${ext}${classifier:+:${classifier}}" >&2
        exit 2
    fi
    printf '%s/%s/%s' "${repo}" "${path}" "${name}"
}

if [ "${PLUGIN_ACTION}" = "download" ]; then
    version="${PLUGIN_VERSION:-}"
    if [ -z "${version}" ]; then
        echo "gocdnext/artifactory: version is required for action=download (or 'LATEST')" >&2
        exit 2
    fi
    if [ "${version}" = "LATEST" ]; then
        repo_rel=$(resolve_latest)
    else
        repo_rel="${repo}/$(maven_path "${version}")"
    fi
    url="${base}/${repo_rel}"

    dest_rel="${PLUGIN_DEST:-.}"
    dest="/workspace/${dest_rel#/}"
    mkdir -p "$(dirname "${dest}/placeholder")"
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
        echo "gocdnext/artifactory: file + upload-version are required for action=upload" >&2
        exit 2
    fi
    src="/workspace/${file_rel#/}"
    if [ ! -f "${src}" ]; then
        echo "gocdnext/artifactory: ${file_rel} not found in workspace" >&2
        exit 2
    fi
    url="${base}/${repo}/$(maven_path "${version}")"
    echo "==> PUT ${url} <- ${src} ($(stat -c%s "${src}" 2>/dev/null || echo "?") bytes)"
    curl -fSsL "${auth_args[@]}" \
        --upload-file "${src}" \
        "${url}"
    echo "==> uploaded"
fi
