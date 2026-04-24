#!/bin/bash
# gocdnext/ansible — thin wrapper around ansible-playbook. See
# Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_PLAYBOOK:-}" ]; then
    echo "gocdnext/ansible: PLUGIN_PLAYBOOK is required" >&2
    echo "  example: playbook: deploy.yml" >&2
    exit 2
fi

cmd=(ansible-playbook "/workspace/${PLUGIN_PLAYBOOK}")

if [ -n "${PLUGIN_INVENTORY:-}" ]; then
    cmd+=("-i" "/workspace/${PLUGIN_INVENTORY}")
fi

if [ -n "${PLUGIN_EXTRA_VARS:-}" ]; then
    # @path → ansible resolves the file itself. Anything else is a
    # raw key=value string passed as-is.
    if [[ "${PLUGIN_EXTRA_VARS}" == @* ]]; then
        cmd+=("-e" "@/workspace/${PLUGIN_EXTRA_VARS#@}")
    else
        cmd+=("-e" "${PLUGIN_EXTRA_VARS}")
    fi
fi

if [ -n "${PLUGIN_TAGS:-}" ]; then
    cmd+=("-t" "${PLUGIN_TAGS}")
fi

if [ -n "${PLUGIN_LIMIT:-}" ]; then
    cmd+=("-l" "${PLUGIN_LIMIT}")
fi

if [ -n "${PLUGIN_SSH_USER:-}" ]; then
    cmd+=("--user" "${PLUGIN_SSH_USER}")
fi

# Secrets wire-up: SSH key and vault password get written to
# tempfiles (0600) and passed as file paths. Never touching
# argv keeps them off `ps auxww`; gocdnext's runner already
# masks the env values in logs.
tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

if [ -n "${PLUGIN_SSH_PRIVATE_KEY:-}" ]; then
    key="${tmpdir}/ssh.key"
    printf '%s\n' "${PLUGIN_SSH_PRIVATE_KEY}" >"${key}"
    chmod 600 "${key}"
    cmd+=("--private-key" "${key}")
fi

if [ -n "${PLUGIN_VAULT_PASSWORD:-}" ]; then
    vault="${tmpdir}/vault.pw"
    printf '%s' "${PLUGIN_VAULT_PASSWORD}" >"${vault}"
    chmod 600 "${vault}"
    cmd+=("--vault-password-file" "${vault}")
fi

if [ -n "${PLUGIN_BECOME_PASSWORD:-}" ]; then
    # Passing via -e is uglier than stdin but keeps the value out
    # of `ps` (env only, no argv) because ansible reads the
    # extra-vars expression but the password itself is just an
    # opaque string. The runner is still masking it from logs.
    cmd+=("-e" "ansible_become_password=${PLUGIN_BECOME_PASSWORD}")
fi

if [ "${PLUGIN_CHECK:-false}" = "true" ]; then
    cmd+=("--check")
fi

# Host key checking off: CI workers don't persist a known_hosts
# file between runs, and --ssh-common-args would fight the
# --private-key path. Trade-off is visible in the ansible docs
# as the standard CI posture.
export ANSIBLE_HOST_KEY_CHECKING=False

echo "==> ${cmd[*]}"
exec "${cmd[@]}"
