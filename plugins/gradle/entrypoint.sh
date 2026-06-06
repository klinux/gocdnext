#!/bin/bash
# gocdnext/gradle — wraps `./gradlew` when present, else `gradle`.
# See Dockerfile for the full contract.

set -euo pipefail

# parse_bool — strict bool normaliser; typos fail loud. Accepted:
# true|1|yes|on, false|0|no|off, case-insensitive. Empty = default.
parse_bool() {
    local name="$1"
    local val="$2"
    local default="$3"
    if [ -z "$val" ]; then
        printf '%s' "$default"
        return 0
    fi
    case "$(printf '%s' "$val" | tr '[:upper:]' '[:lower:]')" in
        true|1|yes|on)   printf 'true' ;;
        false|0|no|off)  printf 'false' ;;
        *)
            echo "gocdnext/gradle: $name accepts true|false|1|0|yes|no|on|off (got '$val')" >&2
            exit 2
            ;;
    esac
}

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/gradle: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: build" >&2
    echo "    command: test" >&2
    echo "    command: publish -Prelease=true" >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

git config --global --add safe.directory '*' 2>/dev/null || true

# Point $GRADLE_USER_HOME at a workspace-relative dir by default
# so the platform's `cache:` block can tar it up. Gradle normally
# writes caches + wrapper JARs under $HOME/.gradle, which sits
# OUTSIDE the workspace the agent tars — the cache key would
# round-trip an empty blob. Override via `variables:
# GRADLE_USER_HOME: ...` in YAML for the rare case someone
# wants a different layout.
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-.gradle-home}"
mkdir -p "${GRADLE_USER_HOME}"

# Gradle wrapper is the best-practice CI entrypoint because it
# pins the exact Gradle version the project was developed
# against — avoids "works on my machine" drift. Fall back to
# the bundled `gradle` binary when a project doesn't ship one.
if [ -x "./gradlew" ]; then
    CLI="./gradlew"
elif command -v gradle >/dev/null 2>&1; then
    CLI="gradle"
else
    echo "gocdnext/gradle: neither ./gradlew nor gradle found" >&2
    exit 2
fi

# Daemon: bi-state with default --no-daemon. CI containers are
# ephemeral; the daemon's "warm JVM next time" payoff doesn't
# apply to a fresh pod per job. Operators with workspace-
# isolation off + a cached $GRADLE_USER_HOME can opt in via
# `daemon: "true"`.
#
# Note `|| exit $?`: parse_bool runs in the subshell created by
# $() and an exit 2 (typo) wouldn't otherwise propagate. The
# `||` clause catches the non-zero subshell exit and re-raises
# it on the outer script.
daemon_val=$(parse_bool daemon "${PLUGIN_DAEMON:-}" "false") || exit $?
if [ "$daemon_val" = "true" ]; then
    daemon_flag="--daemon"
else
    daemon_flag="--no-daemon"
fi

# Build cache — TRI-STATE.
#   unset  → pass no flag; respects the project's
#            `org.gradle.caching=true` (or absence) in
#            gradle.properties.
#   "true" → `--build-cache`
#   "false"→ `--no-build-cache` (force off even if
#            gradle.properties opted in).
#
# Defaults to unset rather than forcing `--no-build-cache` so
# projects already opting in via gradle.properties aren't
# silently overridden by the plugin.
build_cache_flag=""
if [ -n "${PLUGIN_BUILD_CACHE:-}" ]; then
    bc_val=$(parse_bool build-cache "${PLUGIN_BUILD_CACHE}" "") || exit $?
    if [ "$bc_val" = "true" ]; then
        build_cache_flag="--build-cache"
    else
        build_cache_flag="--no-build-cache"
    fi
fi

# Parallel execution — TRI-STATE, same shape as build-cache.
# Respects `org.gradle.parallel=true` in gradle.properties when
# `parallel:` is unset. Explicit "true"/"false" force the flag.
parallel_flag=""
if [ -n "${PLUGIN_PARALLEL:-}" ]; then
    par_val=$(parse_bool parallel "${PLUGIN_PARALLEL}" "") || exit $?
    if [ "$par_val" = "true" ]; then
        parallel_flag="--parallel"
    else
        parallel_flag="--no-parallel"
    fi
fi

# Configuration cache — TRI-STATE.
# `configuration-cache:` unset = no flag (respects
# org.gradle.configuration-cache property). Explicit value
# forces on/off via --configuration-cache /
# --no-configuration-cache.
config_cache_flag=""
if [ -n "${PLUGIN_CONFIGURATION_CACHE:-}" ]; then
    cc_val=$(parse_bool configuration-cache "${PLUGIN_CONFIGURATION_CACHE}" "") || exit $?
    if [ "$cc_val" = "true" ]; then
        config_cache_flag="--configuration-cache"
    else
        config_cache_flag="--no-configuration-cache"
    fi
fi

# JVM args: GRADLE_OPTS controls the launcher JVM (which spawns
# the daemon, even in --no-daemon mode); the JIT + heap defaults
# fit ~80% of projects, but big builds need more.
if [ -n "${PLUGIN_GRADLE_OPTS:-}" ]; then
    export GRADLE_OPTS="${PLUGIN_GRADLE_OPTS}"
fi

# Generic extra args (flags that don't deserve a knob). Passed
# verbatim AFTER the structured flags so they can override.
extra_args="${PLUGIN_ARGS:-}"

# Testcontainers auto-config — ONLY when /var/run/docker.sock is
# mounted (docker engine path). Don't trigger on DOCKER_HOST: K8s
# `docker: true` runs DinD with DOCKER_HOST=tcp://localhost:2375
# and NO socket — explicit overrides would point at a non-existent
# path; Testcontainers' resolver handles DinD natively via
# DOCKER_HOST.
if [ -S /var/run/docker.sock ]; then
    export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="${TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE:-/var/run/docker.sock}"
    export TESTCONTAINERS_HOST_OVERRIDE="${TESTCONTAINERS_HOST_OVERRIDE:-host.docker.internal}"
fi

echo "==> ${CLI} ${daemon_flag} ${build_cache_flag} ${parallel_flag} ${config_cache_flag} ${PLUGIN_COMMAND} ${extra_args}"
# Unquoted expansion so empty flags (tri-state "no flag" cases)
# disappear via word-splitting instead of landing as empty
# positional args that gradle would reject.
# shellcheck disable=SC2086
exec "${CLI}" \
    ${daemon_flag} \
    ${build_cache_flag} \
    ${parallel_flag} \
    ${config_cache_flag} \
    ${PLUGIN_COMMAND} \
    ${extra_args}
