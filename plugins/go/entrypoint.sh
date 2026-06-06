#!/bin/sh
# gocdnext/go entrypoint — thin shim around `go` so pipelines
# don't hand-roll `apk add go && go build` in every job.
#
# Inputs (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  go subcommand + args, word-split.
#   PLUGIN_WORKING_DIR  (optional)  relative path under /workspace.
#   PLUGIN_CGO          (optional)  "true"/"false"; default unset
#                                   (toolchain default). "false" sets
#                                   CGO_ENABLED=0 for cross-compile.
#                                   "true" forces cgo for projects
#                                   that import C deps + want explicit
#                                   audit trail.
#
# Exits with the go CLI's own exit code.

set -eu

# parse_bool normalises a `with:` bool input to "true"/"false" with
# strict validation: typos (`flase`, `tru`, "") fail loudly instead
# of silently picking a wrong default. Accepted: true|1|yes|on,
# false|0|no|off, case-insensitive. Empty = default. POSIX-safe.
parse_bool() {
    name=$1
    val=$2
    default=$3
    if [ -z "$val" ]; then
        printf '%s' "$default"
        return 0
    fi
    case "$(printf '%s' "$val" | tr '[:upper:]' '[:lower:]')" in
        true|1|yes|on)   printf 'true' ;;
        false|0|no|off)  printf 'false' ;;
        *)
            echo "gocdnext/go: $name accepts true|false|1|0|yes|no|on|off (got '$val')" >&2
            exit 2
            ;;
    esac
}

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/go: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: build ./..." >&2
    echo "    command: test -race ./..." >&2
    echo "    command: vet ./..." >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — same workaround as every other
# plugin that hits `git` on the host-cloned workspace.
git config --global --add safe.directory '*' 2>/dev/null || true

# Redirect both Go caches into the workspace so the platform's
# `cache:` block can tar them. GOMODCACHE holds fetched module
# archives (keyed by module@version — huge reuse win), GOCACHE
# holds compiled package artefacts (incremental builds + test
# result memoisation). Base image's defaults sit outside the
# workspace at /root/go + /root/.cache/go-build.
export GOMODCACHE="${GOMODCACHE:-.go-mod}"
export GOCACHE="${GOCACHE:-.go-cache}"
mkdir -p "${GOMODCACHE}" "${GOCACHE}"

# CGO knob. Empty = leave the toolchain default alone (CGO_ENABLED=1
# on linux/amd64 with a C toolchain present, which the alpine base
# image provides via gcc + musl-dev). Explicit "false" disables cgo
# — required for portable cross-compile, common for static release
# binaries. Explicit "true" forces it on (defensive: surfaces a
# clear error rather than silent fallback when the base image
# doesn't carry gcc).
if [ -n "${PLUGIN_CGO:-}" ]; then
    # Capture FIRST, then test. `$(parse_bool ...)` runs in a
    # subshell — an `exit 2` from within only kills the subshell,
    # and the outer `[ "$()" = "true" ]` would silently see an
    # empty string and pick the wrong branch. `|| exit $?`
    # propagates the subshell's status to the parent script.
    cgo_val=$(parse_bool cgo "${PLUGIN_CGO}" "") || exit $?
    if [ "$cgo_val" = "true" ]; then
        export CGO_ENABLED=1
    else
        export CGO_ENABLED=0
    fi
fi

# Testcontainers auto-config — ONLY when the host's /var/run/docker.sock
# is mounted into this container. That's the docker-engine path
# where the agent also wires `host.docker.internal` on the run's
# bridge as the daemon's gateway alias, so both overrides resolve
# correctly.
#
# DO NOT trigger on DOCKER_HOST. In the Kubernetes engine, `docker:
# true` runs a DinD sidecar and sets DOCKER_HOST=tcp://localhost:2375
# with NO socket in the task container — overriding TESTCONTAINERS_
# DOCKER_SOCKET_OVERRIDE to a non-existent path would break tests
# that would otherwise auto-detect via DOCKER_HOST. Testcontainers'
# own resolver handles the DinD case natively.
if [ -S /var/run/docker.sock ]; then
    export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="${TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE:-/var/run/docker.sock}"
    export TESTCONTAINERS_HOST_OVERRIDE="${TESTCONTAINERS_HOST_OVERRIDE:-host.docker.internal}"
fi

# Banner so the log shows what's about to run — `go vet` / `go
# test` against a clean workspace can be silent for many seconds,
# and a silent log reads like the job is hung. Trivy follows the
# same convention.
echo "==> go ${PLUGIN_COMMAND}"

# Word-split intentionally: `build ./...` is two args. If an
# operator needs whitespace inside a single arg, drop back to
# plain `script:`; this plugin optimises the 90% case.
# shellcheck disable=SC2086
exec go ${PLUGIN_COMMAND}
