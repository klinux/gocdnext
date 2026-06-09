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

# TESTCONTAINERS_* env → JAVA_TOOL_OPTIONS bridge.
#
# Operators set `TESTCONTAINERS_RYUK_DISABLED=true` etc. on the
# runner profile so every job inherits the tuning. Those env vars
# reach the Gradle LAUNCHER JVM correctly, but Gradle forks
# separate JVMs to actually run the tests
# (`test.maxParallelForks=3` is the common case), and those forks
# do NOT inherit env vars from the launcher. They inherit
# JAVA_TOOL_OPTIONS though — it's a standard JVM bootstrap knob
# every JVM honours at startup. Testcontainers reads its config
# from EITHER the env var OR the equivalent `-D` system property,
# so this bridge converts:
#
#   TESTCONTAINERS_RYUK_DISABLED=true → -Dtestcontainers.ryuk.disabled=true
#   TESTCONTAINERS_REUSE_ENABLE=true  → -Dtestcontainers.reuse.enable=true
#   TESTCONTAINERS_X_Y_Z=value        → -Dtestcontainers.x.y.z=value
#
# and appends them to JAVA_TOOL_OPTIONS so launcher + daemon + every
# test fork + Kotlin compiler daemon all see the same settings.
# Operator no longer needs `systemProperty` blocks in build.gradle
# to make Testcontainers tuning reach the test JVMs.
#
# Scope is intentionally narrow (TESTCONTAINERS_* prefix only):
# auto-promoting arbitrary env vars to -D flags would leak NEXUS
# secrets and such into `ps` output. The Testcontainers prefix is
# the operator-visible knob the upstream library publishes.
testcontainers_tool_opts=""
while IFS='=' read -r var val; do
    case "$var" in
        TESTCONTAINERS_*)
            [ -z "$val" ] && continue
            # TESTCONTAINERS_RYUK_DISABLED → ryuk.disabled
            suffix="${var#TESTCONTAINERS_}"
            suffix=$(printf '%s' "$suffix" | tr '[:upper:]_' '[:lower:].')
            testcontainers_tool_opts="${testcontainers_tool_opts} -Dtestcontainers.${suffix}=${val}"
            ;;
    esac
done < <(env)

if [ -n "${testcontainers_tool_opts# }" ]; then
    # Preserve any existing JAVA_TOOL_OPTIONS the operator set
    # (e.g., a custom truststore). Bridge args come AFTER so
    # explicit operator settings stay authoritative.
    export JAVA_TOOL_OPTIONS="${JAVA_TOOL_OPTIONS:-}${testcontainers_tool_opts}"
    echo "gocdnext/gradle: bridged TESTCONTAINERS_* env vars to JAVA_TOOL_OPTIONS so they reach test forks"
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
