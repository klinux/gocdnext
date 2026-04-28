#!/bin/bash
# gocdnext/ssh — primitive deploy-over-SSH. See Dockerfile for the
# full input contract.
#
# Failure model: any single sub-step (auth setup, rsync, ssh) that
# returns non-zero aborts the whole job. The remote script runs
# under `set -euo pipefail` so a missing systemctl unit fails the
# CI step instead of a green "deploy" with a dead service.

set -euo pipefail

PLUGIN_HOST="${PLUGIN_HOST:-}"
PLUGIN_USER="${PLUGIN_USER:-}"
PLUGIN_PORT="${PLUGIN_PORT:-22}"
PLUGIN_KEY="${PLUGIN_KEY:-}"
PLUGIN_PASSWORD="${PLUGIN_PASSWORD:-}"
PLUGIN_KNOWN_HOSTS="${PLUGIN_KNOWN_HOSTS:-}"
PLUGIN_HOST_KEY="${PLUGIN_HOST_KEY:-}"
PLUGIN_HOST_KEY_CHECK="${PLUGIN_HOST_KEY_CHECK:-yes}"
PLUGIN_UPLOAD="${PLUGIN_UPLOAD:-}"
PLUGIN_TARGET="${PLUGIN_TARGET:-}"
PLUGIN_RSYNC_OPTS="${PLUGIN_RSYNC_OPTS:--az --delete}"
PLUGIN_SCRIPT="${PLUGIN_SCRIPT:-}"
PLUGIN_WORKING_DIR="${PLUGIN_WORKING_DIR:-}"

if [ -z "$PLUGIN_HOST" ]; then
    echo "gocdnext/ssh: PLUGIN_HOST is required" >&2
    exit 2
fi
if [ -z "$PLUGIN_USER" ]; then
    echo "gocdnext/ssh: PLUGIN_USER is required" >&2
    exit 2
fi
if [ -z "$PLUGIN_KEY" ] && [ -z "$PLUGIN_PASSWORD" ]; then
    echo "gocdnext/ssh: one of PLUGIN_KEY or PLUGIN_PASSWORD is required" >&2
    exit 2
fi
if [ -n "$PLUGIN_KEY" ] && [ -n "$PLUGIN_PASSWORD" ]; then
    echo "gocdnext/ssh: PLUGIN_KEY and PLUGIN_PASSWORD are mutually exclusive" >&2
    exit 2
fi
if [ -n "$PLUGIN_UPLOAD" ] && [ -z "$PLUGIN_TARGET" ]; then
    echo "gocdnext/ssh: PLUGIN_TARGET is required when PLUGIN_UPLOAD is set" >&2
    exit 2
fi
if [ -z "$PLUGIN_UPLOAD" ] && [ -z "$PLUGIN_SCRIPT" ]; then
    echo "gocdnext/ssh: nothing to do — set PLUGIN_UPLOAD, PLUGIN_SCRIPT, or both" >&2
    exit 2
fi

# Workspace for SSH state — outside /workspace so we don't pollute
# the job's source tree, and on /tmp so it's gone with the
# container. mktemp -d so two parallel jobs don't clobber each
# other if the runner ever shares the image.
SSH_HOME="$(mktemp -d -t gocdnext-ssh-XXXXXX)"
chmod 700 "$SSH_HOME"
trap 'rm -rf "$SSH_HOME"' EXIT

KEY_FILE="$SSH_HOME/id"
KNOWN_HOSTS_FILE="$SSH_HOME/known_hosts"

if [ -n "$PLUGIN_KEY" ]; then
    # printf preserves the trailing newline OpenSSH wants on the
    # last line of a PEM-encoded key. echo -e isn't portable across
    # shells, so we lean on printf.
    printf "%s\n" "$PLUGIN_KEY" > "$KEY_FILE"
    chmod 600 "$KEY_FILE"
fi

# Host-key handling. Default = strict; off requires an explicit
# opt-in and prints a warn so it shows up in the run logs.
SSH_HOST_KEY_OPTS=()
case "$PLUGIN_HOST_KEY_CHECK" in
    yes|YES|true|TRUE|1)
        if [ -n "$PLUGIN_KNOWN_HOSTS" ]; then
            printf "%s\n" "$PLUGIN_KNOWN_HOSTS" > "$KNOWN_HOSTS_FILE"
        elif [ -n "$PLUGIN_HOST_KEY" ]; then
            printf "%s\n" "$PLUGIN_HOST_KEY" > "$KNOWN_HOSTS_FILE"
        else
            echo "gocdnext/ssh: host_key_check is on but neither PLUGIN_KNOWN_HOSTS nor PLUGIN_HOST_KEY is set" >&2
            echo "  pass 'ssh-keyscan -p $PLUGIN_PORT $PLUGIN_HOST' output via secrets, or set host_key_check: \"no\" (NOT recommended)." >&2
            exit 2
        fi
        chmod 600 "$KNOWN_HOSTS_FILE"
        SSH_HOST_KEY_OPTS=(
            -o "StrictHostKeyChecking=yes"
            -o "UserKnownHostsFile=$KNOWN_HOSTS_FILE"
        )
        ;;
    no|NO|false|FALSE|0)
        echo "==> WARNING: host_key_check=no — connection is not protected against MITM" >&2
        SSH_HOST_KEY_OPTS=(
            -o "StrictHostKeyChecking=no"
            -o "UserKnownHostsFile=/dev/null"
            -o "LogLevel=ERROR"
        )
        ;;
    *)
        echo "gocdnext/ssh: PLUGIN_HOST_KEY_CHECK = $PLUGIN_HOST_KEY_CHECK (want yes|no)" >&2
        exit 2
        ;;
esac

# Build the canonical SSH option list: identity, host key, port,
# and a tight server-alive interval so a stalled deploy fails fast
# instead of hanging forever on a wedged remote.
SSH_OPTS=(
    -p "$PLUGIN_PORT"
    -o "BatchMode=yes"
    -o "ServerAliveInterval=15"
    -o "ServerAliveCountMax=3"
    "${SSH_HOST_KEY_OPTS[@]}"
)
if [ -n "$PLUGIN_KEY" ]; then
    # IdentitiesOnly + IdentityFile pair: ignore any agent that
    # might be plumbed into the container, so the connection
    # uses ONLY the key we just wrote.
    SSH_OPTS+=(
        -i "$KEY_FILE"
        -o "IdentitiesOnly=yes"
    )
fi

# Wrapper that prepends sshpass when password auth is in use.
# Both ssh and rsync invocations route through this so the same
# auth handling applies to file copy and remote exec.
ssh_with_auth() {
    if [ -n "$PLUGIN_PASSWORD" ]; then
        SSHPASS="$PLUGIN_PASSWORD" sshpass -e ssh "${SSH_OPTS[@]}" "$@"
    else
        ssh "${SSH_OPTS[@]}" "$@"
    fi
}

# Banner before any work so a silent SSH (e.g. the upload completes
# without remote output) still produces a visible log line.
echo "==> ssh ${PLUGIN_USER}@${PLUGIN_HOST}:${PLUGIN_PORT}"

if [ -n "$PLUGIN_WORKING_DIR" ]; then
    cd "/workspace/$PLUGIN_WORKING_DIR" 2>/dev/null || cd "$PLUGIN_WORKING_DIR"
else
    cd /workspace 2>/dev/null || true
fi

if [ -n "$PLUGIN_UPLOAD" ]; then
    # Accept newline OR comma as separator — newline is YAML-block
    # natural, comma is more compact for one-line `upload:` values.
    UPLOAD_TMP="$(printf "%s" "$PLUGIN_UPLOAD" | tr ',\n' '\n\n' | sed '/^$/d')"
    UPLOAD_PATHS=()
    while IFS= read -r p; do
        [ -n "$p" ] && UPLOAD_PATHS+=("$p")
    done <<< "$UPLOAD_TMP"

    if [ "${#UPLOAD_PATHS[@]}" -eq 0 ]; then
        echo "gocdnext/ssh: PLUGIN_UPLOAD parsed to zero paths" >&2
        exit 2
    fi

    echo "==> rsync ${UPLOAD_PATHS[*]} -> ${PLUGIN_USER}@${PLUGIN_HOST}:${PLUGIN_TARGET}"
    # rsync's `-e` lets us reuse the SSH wrapper. We add --mkpath
    # so the remote target dir doesn't need to exist (rsync 3.2.3+).
    if [ -n "$PLUGIN_PASSWORD" ]; then
        SSHPASS="$PLUGIN_PASSWORD" sshpass -e rsync \
            $PLUGIN_RSYNC_OPTS \
            --mkpath \
            -e "ssh ${SSH_OPTS[*]}" \
            "${UPLOAD_PATHS[@]}" \
            "${PLUGIN_USER}@${PLUGIN_HOST}:${PLUGIN_TARGET}"
    else
        rsync \
            $PLUGIN_RSYNC_OPTS \
            --mkpath \
            -e "ssh ${SSH_OPTS[*]}" \
            "${UPLOAD_PATHS[@]}" \
            "${PLUGIN_USER}@${PLUGIN_HOST}:${PLUGIN_TARGET}"
    fi
fi

if [ -n "$PLUGIN_SCRIPT" ]; then
    echo "==> remote script"
    # The remote shell runs strict mode so a missing command kills
    # the deploy instead of marching past it. We send the script on
    # stdin (rather than as an argv string) to dodge quoting hell
    # when it spans multiple lines.
    ssh_with_auth "${PLUGIN_USER}@${PLUGIN_HOST}" \
        "bash -se" <<EOF
set -euo pipefail
$PLUGIN_SCRIPT
EOF
fi

echo "==> ssh: done"
