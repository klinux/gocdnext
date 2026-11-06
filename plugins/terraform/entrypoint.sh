#!/bin/bash
# gocdnext/terraform — terraform wrapper with exact-version selection.

set -euo pipefail

fail() {
    echo "gocdnext/terraform: $1" >&2
    exit 2
}

[ -n "${PLUGIN_COMMAND:-}" ] || fail "command is required (example: plan -out=tfplan)"

cd "${PLUGIN_WORKING_DIR:-.}"
git config --global --add safe.directory '*' 2>/dev/null || true

# Split on whitespace by contract, but execute an array: unlike unquoted
# expansion this does not unexpectedly expand workspace globs.
read -ra CMD_PARTS <<<"${PLUGIN_COMMAND}"
SUBCMD="${CMD_PARTS[0]:-}"
[ -n "${SUBCMD}" ] || fail "command is empty"

var_file_args=()
if [ -n "${PLUGIN_VAR_FILE:-}" ]; then
    case "${SUBCMD}" in
        plan|apply|destroy|refresh|import)
            var_file_args+=("-var-file" "${PLUGIN_VAR_FILE}")
            ;;
        *)
            echo "==> ignoring var-file for '${SUBCMD}' (not supported by this subcommand)"
            ;;
    esac
fi

bundled_version=1.17.1
requested="${PLUGIN_VERSION:-latest}"
terraform_bin="${BUNDLED_TERRAFORM_BIN:-/bin/terraform}"

case "${requested}" in
    latest|"${bundled_version}")
        # `latest` deliberately means the release baked into this immutable
        # image, not a moving network lookup.
        ;;
    *)
        [[ "${requested}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || \
            fail "version must be latest or an exact stable version (for example 1.8.5; got '${requested}')"

        export TFENV_CONFIG_DIR="${TFENV_CONFIG_DIR:-/tmp/gocdnext-tfenv}"
        export TFENV_TERRAFORM_VERSION="${requested}"
        export TFENV_SKIP_REMOTE_CHECK="${TFENV_SKIP_REMOTE_CHECK:-1}"
        export TFENV_CURL_OUTPUT="${TFENV_CURL_OUTPUT:-0}"
        mkdir -p "${TFENV_CONFIG_DIR}"

        tfenv_bin="${TFENV_BIN:-/opt/tfenv/bin/tfenv}"
        echo "==> installing Terraform ${requested} (verified SHA-256)"
        "${tfenv_bin}" install "${requested}"
        terraform_bin="${TFENV_CONFIG_DIR}/versions/${requested}/terraform"
        [ -x "${terraform_bin}" ] || fail "tfenv did not install executable ${terraform_bin}"
        ;;
esac

echo "==> terraform ${PLUGIN_COMMAND} ${var_file_args[*]:-} (version=${requested})"
exec "${terraform_bin}" "${CMD_PARTS[@]}" "${var_file_args[@]}"
