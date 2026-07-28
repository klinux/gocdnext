#!/bin/sh
# gocdnext/npm-publish entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/npm-publish: $1" >&2; exit 2; }

DRY_RUN="$(printf '%s' "${PLUGIN_DRY_RUN:-false}" | tr '[:upper:]' '[:lower:]')"
IF_EXISTS="$(printf '%s' "${PLUGIN_IF_EXISTS:-fail}" | tr '[:upper:]' '[:lower:]')"
case "${IF_EXISTS}" in fail|skip) ;; *) fail "if-exists must be fail | skip (got '${PLUGIN_IF_EXISTS}')";; esac

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi
DIR="${PLUGIN_DIR:-.}"
[ -f "${DIR}/package.json" ] || fail "no package.json in '${DIR}'"

REGISTRY="${PLUGIN_REGISTRY:-https://registry.npmjs.org}"
REGISTRY="${REGISTRY%/}"

# Fail-closed: a registry URL with embedded userinfo (user:pass@host) would leak
# into .npmrc, the --registry argv, AND the progress log below — reject it. The
# credential goes through auth/username/password or NPM_TOKEN, never the URL.
REG_HOST_PATH="${REGISTRY#*//}"   # host+path, no scheme; the .npmrc key adds the trailing slash
case "${REG_HOST_PATH%%/*}" in
    *@*) fail "registry URL must not embed credentials (user:pass@host); use auth/username/password or NPM_TOKEN" ;;
esac

NAME=$(jq -r '.name // empty' "${DIR}/package.json")
VERSION=$(jq -r '.version // empty' "${DIR}/package.json")
[ -n "${NAME}" ] && [ -n "${VERSION}" ] || fail "package.json must carry name + version"

# Auth mode: `token` (NPM_TOKEN, e.g. npmjs / GitHub Packages) or `basic`
# (username+password, e.g. Nexus / Artifactory / Verdaccio and other private npm
# registries). `auto` (default) picks basic when username+password are both set,
# else token. A dry-run packs without auth.
AUTH_MODE="$(printf '%s' "${PLUGIN_AUTH:-auto}" | tr '[:upper:]' '[:lower:]')"
case "${AUTH_MODE}" in auto|token|basic) ;; *) fail "auth must be auto | token | basic (got '${PLUGIN_AUTH}')";; esac
if [ "${AUTH_MODE}" = "auto" ]; then
    if [ -n "${PLUGIN_USERNAME:-}" ] && [ -n "${PLUGIN_PASSWORD:-}" ]; then
        AUTH_MODE="basic"
    else
        AUTH_MODE="token"
    fi
fi

# Real publishes write a USER-scoped ~/.npmrc — the credential never touches
# argv or the workspace (a workspace .npmrc could be archived by artifacts).
# always-auth=true so the private-registry existence check (npm view) and the
# publish both authenticate.
if [ "${DRY_RUN}" != "true" ]; then
    NPMRC="${HOME}/.npmrc"
    case "${AUTH_MODE}" in
        token)
            [ -n "${NPM_TOKEN:-}" ] || fail "auth=token needs NPM_TOKEN (secrets: [NPM_TOKEN])"
            {
                printf '//%s/:_authToken=%s\n' "${REG_HOST_PATH}" "${NPM_TOKEN}"
                printf '//%s/:always-auth=true\n' "${REG_HOST_PATH}"
            } > "${NPMRC}"
            ;;
        basic)
            { [ -n "${PLUGIN_USERNAME:-}" ] && [ -n "${PLUGIN_PASSWORD:-}" ]; } \
                || fail "auth=basic needs username + password (with: username/password)"
            # _auth = base64(user:pass); computed here, written straight to the
            # .npmrc, never echoed — same discipline as the token path.
            _auth_b64="$(printf '%s:%s' "${PLUGIN_USERNAME}" "${PLUGIN_PASSWORD}" | base64 | tr -d '\n')"
            {
                printf '//%s/:_auth=%s\n' "${REG_HOST_PATH}" "${_auth_b64}"
                printf '//%s/:always-auth=true\n' "${REG_HOST_PATH}"
            } > "${NPMRC}"
            unset _auth_b64
            ;;
    esac
    chmod 600 "${NPMRC}"
fi

# Idempotency: name@version is immutable on npm — a retried
# pipeline that already published must not fail the whole run.
if [ "${IF_EXISTS}" = "skip" ] && [ "${DRY_RUN}" != "true" ]; then
    if npm view "${NAME}@${VERSION}" version --registry "${REGISTRY}" >/dev/null 2>&1; then
        echo "==> ${NAME}@${VERSION} already published — if-exists: skip, nothing to do"
        exit 0
    fi
fi

set -- publish --registry "${REGISTRY}"
[ -n "${PLUGIN_TAG:-}" ] && set -- "$@" --tag "${PLUGIN_TAG}"
[ -n "${PLUGIN_ACCESS:-}" ] && set -- "$@" --access "${PLUGIN_ACCESS}"
[ "${DRY_RUN}" = "true" ] && set -- "$@" --dry-run

echo "==> npm publish ${NAME}@${VERSION} (registry=${REGISTRY}${PLUGIN_TAG:+ tag=${PLUGIN_TAG}}${DRY_RUN:+ dry-run=${DRY_RUN}})"
cd "${DIR}"
exec npm "$@"
