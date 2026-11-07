#!/bin/bash
# gocdnext/helm-push — package + push a Helm chart. See
# Dockerfile for the full contract.

set -euo pipefail

chart_dir="${PLUGIN_CHART_DIR:-.}"
src="${chart_dir#/}"
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

# --- curl credentials via a 0600 --config file (NEVER argv) ------------------
# `curl --user u:p` puts the credential in /proc/<pid>/cmdline, readable by any
# process sharing the PID namespace. For the chartmuseum/nexus backends the
# creds go into a 0600 file in a 0700 dir instead, handed to curl via --config.
# The values are escaped for curl's double-quoted config grammar so a password
# containing a quote, backslash or newline can't inject a second curl option
# (that escaping IS the injection defence). The file is removed on exit by the
# trap — so the curl calls below do NOT `exec` (exec would skip the trap).
AUTH_CFG=""
AUTH_DIR=""
cleanup_auth() {
    [ -n "${AUTH_CFG}" ] && rm -f "${AUTH_CFG}" 2>/dev/null
    [ -n "${AUTH_DIR}" ] && rm -rf "${AUTH_DIR}" 2>/dev/null
    return 0
}
trap cleanup_auth EXIT

curl_escape() { # escape a value for curl's double-quoted config value
    local s="$1"
    s="${s//\\/\\\\}"     # backslash first, so later escapes aren't doubled
    s="${s//\"/\\\"}"     # double quote
    s="${s//$'\n'/\\n}"   # LF  -> \n   (can't split the line)
    s="${s//$'\r'/\\r}"   # CR  -> \r
    s="${s//$'\t'/\\t}"   # TAB -> \t
    printf '%s' "${s}"
}

# write_auth_cfg: if a username is set, write `user = "u:p"` to a private
# --config file and set auth_args=(--config <file>); else auth_args=().
write_auth_cfg() {
    auth_args=()
    [ -z "${PLUGIN_USERNAME:-}" ] && return 0
    AUTH_DIR="$(mktemp -d)"
    chmod 700 "${AUTH_DIR}"
    AUTH_CFG="${AUTH_DIR}/auth"
    ( umask 077
      printf 'user = "%s:%s"\n' \
          "$(curl_escape "${PLUGIN_USERNAME}")" \
          "$(curl_escape "${PLUGIN_PASSWORD:-}")" >"${AUTH_CFG}" )
    auth_args=(--config "${AUTH_CFG}")
}

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
        # the upload + optional basic auth (via the --config file).
        write_auth_cfg
        endpoint="${repo_url%/}/api/charts"
        echo "==> POST ${endpoint} <- $(basename "${tgz}")"
        # No `exec`: the EXIT trap must run to shred the auth file.
        curl -fSsL "${auth_args[@]}" \
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
        write_auth_cfg
        target="${repo_url%/}/$(basename "${tgz}")"
        echo "==> PUT ${target}"
        # No `exec`: the EXIT trap must run to shred the auth file.
        curl -fSsL "${auth_args[@]}" \
            --upload-file "${tgz}" \
            "${target}"
        ;;

    *)
        echo "gocdnext/helm-push: unknown backend '${backend}' (accepted: oci, chartmuseum, nexus)" >&2
        exit 2
        ;;
esac
