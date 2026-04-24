#!/bin/bash
# gocdnext/terraform — thin wrapper around `terraform`. See
# Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/terraform: PLUGIN_COMMAND is required" >&2
    echo "  example: command: plan -out=tfplan" >&2
    exit 2
fi

WORKING_DIR="${PLUGIN_WORKING_DIR:-.}"
cd "/workspace/${WORKING_DIR}"

# Subcommand is the first word of the command — we need it to
# decide whether PLUGIN_VAR_FILE applies (init/plan/apply/destroy
# take -var-file; fmt/validate/output don't).
read -ra CMD_PARTS <<<"${PLUGIN_COMMAND}"
SUBCMD="${CMD_PARTS[0]:-}"

var_file_args=()
if [ -n "${PLUGIN_VAR_FILE:-}" ]; then
    case "${SUBCMD}" in
        plan|apply|destroy|refresh|import)
            var_file_args+=("-var-file" "/workspace/${PLUGIN_VAR_FILE}")
            ;;
        *)
            echo "==> ignoring var-file for '${SUBCMD}' (not supported by this subcommand)"
            ;;
    esac
fi

# Trust the workspace root for git — same dubious-ownership
# workaround as the other plugins.
git config --global --add safe.directory '*' 2>/dev/null || true

# Word-split PLUGIN_COMMAND on purpose: "plan -out=tfplan" should
# reach terraform as two args, not one.
# shellcheck disable=SC2086
echo "==> terraform ${PLUGIN_COMMAND} ${var_file_args[*]:-}"
# shellcheck disable=SC2086
exec terraform ${PLUGIN_COMMAND} "${var_file_args[@]}"
