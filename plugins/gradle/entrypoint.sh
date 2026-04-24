#!/bin/bash
# gocdnext/gradle — wraps `./gradlew` when present, else `gradle`.
# See Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/gradle: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: build" >&2
    echo "    command: test" >&2
    echo "    command: publish -Prelease=true" >&2
    exit 2
fi

cd /workspace
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
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-/workspace/.gradle-home}"
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

daemon_flag="--no-daemon"
if [ "${PLUGIN_DAEMON:-false}" = "true" ]; then
    daemon_flag="--daemon"
fi

if [ -n "${PLUGIN_GRADLE_OPTS:-}" ]; then
    export GRADLE_OPTS="${PLUGIN_GRADLE_OPTS}"
fi

echo "==> ${CLI} ${daemon_flag} ${PLUGIN_COMMAND}"
# shellcheck disable=SC2086
exec "${CLI}" "${daemon_flag}" ${PLUGIN_COMMAND}
