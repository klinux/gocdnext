#!/bin/bash
# gocdnext/maven — thin wrapper that optionally synthesises a
# settings.xml from PLUGIN_NEXUS_* env so operators don't have to
# check in credentials. See Dockerfile for the full contract.

set -euo pipefail

# Honor the `jdk:` input (PLUGIN_JDK) BEFORE any JVM tool is
# spawned. select-jdk.sh ships with the jdk-base image and is on
# PATH at /usr/local/bin/select-jdk.sh. Source (don't exec) so
# JAVA_HOME + PATH land in this shell for the mvn launcher.
. /usr/local/bin/select-jdk.sh

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
            echo "gocdnext/maven: $name accepts true|false|1|0|yes|no|on|off (got '$val')" >&2
            exit 2
            ;;
    esac
}

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/maven: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: verify" >&2
    echo "    command: clean package -DskipTests" >&2
    echo "    command: deploy -Pprod" >&2
    exit 2
fi

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

git config --global --add safe.directory '*' 2>/dev/null || true

# Point Maven's local repository at a workspace-relative dir by
# default so the platform's `cache:` block can tar it between
# runs. Maven normally writes to ~/.m2/repository which sits
# OUTSIDE the workspace the agent tars. The -Dmaven.repo.local
# flag on the CLI wins over settings.xml, so this applies
# universally. Override via `variables: MAVEN_LOCAL_REPO: ...`
# in YAML when a custom layout is needed.
export MAVEN_LOCAL_REPO="${MAVEN_LOCAL_REPO:-.m2-repo}"
mkdir -p "${MAVEN_LOCAL_REPO}"
local_repo_arg=("-Dmaven.repo.local=${MAVEN_LOCAL_REPO}")

# MAVEN_OPTS controls Maven's JVM args — heap size, GC, network
# tuning. The most-requested knob is `-Xmx` for big multi-module
# reactors. Default base image leaves it at JDK default (~256MB),
# which dies fast on a hundred-module build.
if [ -n "${PLUGIN_MAVEN_OPTS:-}" ]; then
    export MAVEN_OPTS="${PLUGIN_MAVEN_OPTS}"
fi

# Parallel build flag. "1C" = one thread per CPU core (Maven's
# autodetection); explicit numbers (e.g. "4") cap thread count.
# Most modern multi-module reactors are configured to tolerate
# parallel builds; legacy reactors that share state across modules
# may need to stay serial.
parallel_args=()
if [ -n "${PLUGIN_PARALLEL:-}" ]; then
    parallel_args+=("-T" "${PLUGIN_PARALLEL}")
fi

# Maven Build Cache Extension (opt-in). When enabled, Maven skips
# rebuilding modules whose inputs are unchanged (Gradle-style task
# memo). Configure the extension once in the project's
# .mvn/extensions.xml and gate it from CI with this flag — when
# `false` the entrypoint passes -Dmaven.build.cache.enabled=false
# so the extension stays inert for `deploy` runs (where you want
# a fresh build). Empty = leave the extension's own default in
# place (typically on when registered).
build_cache_args=()
if [ -n "${PLUGIN_BUILD_CACHE:-}" ]; then
    # Capture first; `$(parse_bool ...)`'s exit lives in a
    # subshell so a typo would otherwise silently fall through.
    bc_val=$(parse_bool build-cache "${PLUGIN_BUILD_CACHE}" "") || exit $?
    if [ "$bc_val" = "true" ]; then
        build_cache_args+=("-Dmaven.build.cache.enabled=true")
    else
        build_cache_args+=("-Dmaven.build.cache.enabled=false")
    fi
fi

# Colour — force ANSI so BUILD SUCCESS/FAILURE + errors show in
# colour. CI has no TTY and we pass --batch-mode, so Maven's `auto`
# default prints everything white. Default "true" → always; "false"
# → never; "auto" → leave it to Maven. -Dstyle.color=always wins
# over --batch-mode's implicit off.
color_args=()
color_val=$(printf '%s' "${PLUGIN_COLOR:-true}" | tr '[:upper:]' '[:lower:]')
case "$color_val" in
    true|1|yes|on)   color_args+=("-Dstyle.color=always") ;;
    false|0|no|off)  color_args+=("-Dstyle.color=never") ;;
    auto)            ;; # leave to Maven's own detection
    *)
        echo "gocdnext/maven: color accepts true|false|auto (got '${PLUGIN_COLOR:-}')" >&2
        exit 2
        ;;
esac

settings_arg=()
if [ -n "${PLUGIN_SETTINGS:-}" ]; then
    # Operator-provided settings.xml wins — they probably
    # already encode the repositories, mirrors, and policies
    # they care about.
    settings_arg+=("--settings" "${PLUGIN_SETTINGS}")
elif [ -n "${PLUGIN_NEXUS_USERNAME:-}" ] && [ -n "${PLUGIN_NEXUS_PASSWORD:-}" ]; then
    # Synthesised shape: two <server> entries so both snapshot
    # and release IDs resolve without the operator maintaining
    # the file by hand. Written to /tmp so a re-run doesn't
    # pollute the workspace (and so credentials don't leak into
    # artifact uploads).
    snap="${PLUGIN_SNAPSHOT_REPO_ID:-snapshots}"
    rel="${PLUGIN_RELEASE_REPO_ID:-releases}"
    cat >/tmp/gocdnext-maven-settings.xml <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <servers>
    <server>
      <id>${snap}</id>
      <username>${PLUGIN_NEXUS_USERNAME}</username>
      <password>${PLUGIN_NEXUS_PASSWORD}</password>
    </server>
    <server>
      <id>${rel}</id>
      <username>${PLUGIN_NEXUS_USERNAME}</username>
      <password>${PLUGIN_NEXUS_PASSWORD}</password>
    </server>
  </servers>
</settings>
EOF
    chmod 0600 /tmp/gocdnext-maven-settings.xml
    settings_arg+=("--settings" "/tmp/gocdnext-maven-settings.xml")
fi

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

echo "==> mvn ${PLUGIN_COMMAND} (repo.local=${MAVEN_LOCAL_REPO}${PLUGIN_PARALLEL:+, parallel=${PLUGIN_PARALLEL}}${PLUGIN_BUILD_CACHE:+, build-cache=${PLUGIN_BUILD_CACHE}})"
# `--no-transfer-progress` kills the "Downloading (12 KB of 25 MB)"
# spam from cold runs that dumps tens of thousands of log lines.
# Standard CI hygiene; pairs with --batch-mode.
# shellcheck disable=SC2086
exec mvn --batch-mode --no-transfer-progress \
    "${color_args[@]}" \
    "${local_repo_arg[@]}" \
    "${parallel_args[@]}" \
    "${build_cache_args[@]}" \
    "${settings_arg[@]}" \
    ${PLUGIN_COMMAND}
