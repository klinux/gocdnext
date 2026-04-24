#!/bin/bash
# gocdnext/helm-push — package + push a Helm chart. See
# Dockerfile for the full contract.

set -euo pipefail

chart_dir="${PLUGIN_CHART_DIR:-.}"
src="/workspace/${chart_dir#/}"
if [ ! -f "${src}/Chart.yaml" ]; then
    echo "gocdnext/helm-push: no Chart.yaml at ${chart_dir}" >&2
    exit 2
fi

backend="${PLUGIN_BACKEND:-oci}"

# Package stage: produces <chartname>-<version>.tgz at /tmp/pkg/.
# Package once, push once — keeps the logic per-backend below
# focused on transport, not bundling.
pkg_dir="/tmp/gocdnext-helm-pkg"
rm -rf "${pkg_dir}" && mkdir -p "${pkg_dir}"

pkg_args=("${src}" --destination "${pkg_dir}")
if [ -n "${PLUGIN_VERSION:-}" ]; then
    pkg_args+=(--version "${PLUGIN_VERSION}")
fi
if [ -n "${PLUGIN_APP_VERSION:-}" ]; then
    pkg_args+=(--app-version "${PLUGIN_APP_VERSION}")
fi

echo "==> helm package ${src}"
helm package "${pkg_args[@]}" >/dev/null

# The tgz filename is <name>-<version>.tgz; grab whatever landed.
tgz=$(ls -1 "${pkg_dir}"/*.tgz | head -n 1)
if [ -z "${tgz}" ] || [ ! -f "${tgz}" ]; then
    echo "gocdnext/helm-push: helm package didn't produce a tgz" >&2
    exit 2
fi
echo "==> packaged $(basename "${tgz}") ($(stat -c%s "${tgz}" 2>/dev/null || echo "?") bytes)"

case "${backend}" in
    oci)
        oci_repo="${PLUGIN_OCI_REPO:-}"
        if [ -z "${oci_repo}" ]; then
            echo "gocdnext/helm-push: oci-repo is required for backend=oci" >&2
            exit 2
        fi
        if [ -n "${PLUGIN_USERNAME:-}" ]; then
            # Registry host is whatever follows oci:// up to the
            # first path segment — helm registry login expects
            # the plain host, not the path.
            host="${oci_repo#oci://}"
            host="${host%%/*}"
            echo "==> helm registry login ${host}"
            printf '%s' "${PLUGIN_PASSWORD:-}" \
                | helm registry login --username "${PLUGIN_USERNAME}" --password-stdin "${host}"
        fi
        echo "==> helm push ${tgz} ${oci_repo}"
        exec helm push "${tgz}" "${oci_repo}"
        ;;

    chartmuseum)
        repo_url="${PLUGIN_REPO_URL:-}"
        if [ -z "${repo_url}" ]; then
            echo "gocdnext/helm-push: repo-url is required for backend=chartmuseum" >&2
            exit 2
        fi
        # ChartMuseum's HTTP API accepts a plain POST with the
        # chart body. No helm subcommand needed; curl handles
        # the upload + optional basic auth in one line.
        auth_args=()
        if [ -n "${PLUGIN_USERNAME:-}" ]; then
            auth_args=(--user "${PLUGIN_USERNAME}:${PLUGIN_PASSWORD:-}")
        fi
        endpoint="${repo_url%/}/api/charts"
        echo "==> POST ${endpoint} <- $(basename "${tgz}")"
        exec curl -fSsL "${auth_args[@]}" \
            --data-binary "@${tgz}" \
            "${endpoint}"
        ;;

    nexus)
        repo_url="${PLUGIN_REPO_URL:-}"
        if [ -z "${repo_url}" ]; then
            echo "gocdnext/helm-push: repo-url is required for backend=nexus" >&2
            exit 2
        fi
        # Nexus hosted-helm repos accept a plain PUT with the
        # tgz. The URL needs the trailing filename so Nexus
        # can index the chart correctly.
        auth_args=()
        if [ -n "${PLUGIN_USERNAME:-}" ]; then
            auth_args=(--user "${PLUGIN_USERNAME}:${PLUGIN_PASSWORD:-}")
        fi
        target="${repo_url%/}/$(basename "${tgz}")"
        echo "==> PUT ${target}"
        exec curl -fSsL "${auth_args[@]}" \
            --upload-file "${tgz}" \
            "${target}"
        ;;

    *)
        echo "gocdnext/helm-push: unknown backend '${backend}' (accepted: oci, chartmuseum, nexus)" >&2
        exit 2
        ;;
esac
