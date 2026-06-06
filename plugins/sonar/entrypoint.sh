#!/bin/bash
# gocdnext/sonar — Sonar scanner front-end. See Dockerfile for
# the full contract.

set -euo pipefail

# parse_bool — same shape as the go/maven/gradle plugins.
# Accepts true|false|1|0|yes|no|on|off, case-insensitive. Empty =
# default. POSIX-safe (apart from `local`, which bash provides).
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
            echo "gocdnext/sonar: $name accepts true|false|1|0|yes|no|on|off (got '$val')" >&2
            exit 2
            ;;
    esac
}

# --- required inputs --------------------------------------------
TOKEN="${PLUGIN_TOKEN:-${SONAR_TOKEN:-}}"
if [ -z "${TOKEN}" ]; then
    echo "gocdnext/sonar: PLUGIN_TOKEN (or SONAR_TOKEN env) is required" >&2
    echo "  pass via the job's secrets: [SONAR_TOKEN], then either" >&2
    echo "  inject env (SONAR_TOKEN is read by the scanner directly)" >&2
    echo "  or set token: \${{ SONAR_TOKEN }} in with:" >&2
    exit 2
fi
# Export so the scanner picks it up via env (avoids -Dsonar.token=
# on the CLI which would land the value in `ps auxww`).
export SONAR_TOKEN="${TOKEN}"

PROJECT_KEY="${PLUGIN_PROJECT_KEY:-}"
if [ -z "${PROJECT_KEY}" ]; then
    echo "gocdnext/sonar: PLUGIN_PROJECT_KEY is required" >&2
    echo "  set with: { project-key: my-org_my-app }" >&2
    exit 2
fi

# --- optional inputs / defaults ---------------------------------
HOST_URL="${PLUGIN_HOST_URL:-https://sonarcloud.io}"
ORGANIZATION="${PLUGIN_ORGANIZATION:-}"
PROJECT_NAME="${PLUGIN_PROJECT_NAME:-${PROJECT_KEY}}"
# Project version defaults to short SHA for a stable audit trail.
# Empty if the run carries no revision (manual + no-material).
PROJECT_VERSION="${PLUGIN_PROJECT_VERSION:-${CI_COMMIT_SHORT_SHA:-}}"

# SonarCloud requires sonar.organization. Reject early so the
# operator sees a clear local error instead of a server-side
# "missing organization" reply.
case "${HOST_URL}" in
    *sonarcloud.io*)
        if [ -z "${ORGANIZATION}" ]; then
            echo "gocdnext/sonar: organization: is required for SonarCloud" >&2
            echo "  example: organization: my-org" >&2
            exit 2
        fi
        ;;
esac

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — same workaround as every other
# plugin that hits `git` on the host-cloned workspace. Sonar
# reads git history for blame attribution (sonar.scm.provider=git).
git config --global --add safe.directory '*' 2>/dev/null || true

# --- mode auto-detect -------------------------------------------
MODE="${PLUGIN_MODE:-auto}"
case "${MODE}" in
    auto)
        if [ -f pom.xml ]; then
            MODE=maven
        elif [ -f build.gradle ] || [ -f build.gradle.kts ]; then
            MODE=gradle
        else
            MODE=scanner-cli
        fi
        ;;
    maven|gradle|scanner-cli)
        : # accepted as-is
        ;;
    *)
        echo "gocdnext/sonar: mode accepts auto|maven|gradle|scanner-cli (got '${MODE}')" >&2
        exit 2
        ;;
esac

# --- workspace-local caches -------------------------------------
# All cache dirs anchor at the workspace ROOT (`/workspace`), not
# at the project's cwd. The plugin does `cd "${PLUGIN_WORKING_DIR}"`
# above to put the build in the right module; if cache dirs were
# relative paths they'd land inside the module dir (e.g.
# `/workspace/apps/web/.m2-repo`), which doesn't match the
# `cache: paths: [.m2-repo]` convention (workspace-relative). The
# result would be a silent cache miss on every monorepo run.
# Absolute /workspace paths sidestep that — the agent's cache
# tar still captures `.m2-repo` at the workspace root regardless
# of which module the scan ran from.
export SONAR_USER_HOME="${SONAR_USER_HOME:-/workspace/.sonar-cache}"
mkdir -p "${SONAR_USER_HOME}"

if [ "${MODE}" = "maven" ]; then
    export MAVEN_LOCAL_REPO="${MAVEN_LOCAL_REPO:-/workspace/.m2-repo}"
    mkdir -p "${MAVEN_LOCAL_REPO}"
elif [ "${MODE}" = "gradle" ]; then
    export GRADLE_USER_HOME="${GRADLE_USER_HOME:-/workspace/.gradle-home}"
    mkdir -p "${GRADLE_USER_HOME}"
fi

# --- Quality Gate wait ------------------------------------------
# When `wait-for-quality-gate: "true"`, the scanner blocks until
# the server has evaluated the gate (`sonar.qualitygate.wait`)
# and exits non-zero if the gate fails. Useful for PR-blocking
# pipelines; default off because the wait adds ~1-3 minutes of
# polling.
WAIT_QG=$(parse_bool wait-for-quality-gate "${PLUGIN_WAIT_FOR_QUALITY_GATE:-}" "false") || exit $?
QG_TIMEOUT="${PLUGIN_QUALITY_GATE_TIMEOUT:-300}"

# --- assemble -Dsonar.* properties ------------------------------
sonar_props=(
    "-Dsonar.host.url=${HOST_URL}"
    "-Dsonar.projectKey=${PROJECT_KEY}"
    "-Dsonar.projectName=${PROJECT_NAME}"
)
[ -n "${PROJECT_VERSION}" ] && sonar_props+=("-Dsonar.projectVersion=${PROJECT_VERSION}")
[ -n "${ORGANIZATION}" ]    && sonar_props+=("-Dsonar.organization=${ORGANIZATION}")
[ -n "${PLUGIN_SOURCES:-}" ] && sonar_props+=("-Dsonar.sources=${PLUGIN_SOURCES}")
[ -n "${PLUGIN_TESTS:-}" ]   && sonar_props+=("-Dsonar.tests=${PLUGIN_TESTS}")

# Branch vs PR are mutually exclusive on the server side. PR mode
# wins when pull-request-key is set; branch.name is only emitted
# when neither PR mode nor empty.
if [ -n "${PLUGIN_PULL_REQUEST_KEY:-}" ]; then
    sonar_props+=("-Dsonar.pullrequest.key=${PLUGIN_PULL_REQUEST_KEY}")
    [ -n "${PLUGIN_PULL_REQUEST_BRANCH:-}" ] && \
        sonar_props+=("-Dsonar.pullrequest.branch=${PLUGIN_PULL_REQUEST_BRANCH}")
    [ -n "${PLUGIN_PULL_REQUEST_BASE:-}" ] && \
        sonar_props+=("-Dsonar.pullrequest.base=${PLUGIN_PULL_REQUEST_BASE}")
elif [ -n "${PLUGIN_BRANCH:-}" ]; then
    # NOTE: branch analysis is a paid SonarQube feature
    # (Developer Edition+); Community Edition rejects this
    # property at server-side. SonarCloud allows it always.
    sonar_props+=("-Dsonar.branch.name=${PLUGIN_BRANCH}")
fi

if [ "${WAIT_QG}" = "true" ]; then
    sonar_props+=("-Dsonar.qualitygate.wait=true")
    sonar_props+=("-Dsonar.qualitygate.timeout=${QG_TIMEOUT}")
fi

# Free-form extra props pass-through. Parsed LINE-BY-LINE so a
# value containing whitespace ("-Dsonar.projectDescription=my app")
# stays a single argv. Two things are rejected to keep the plugin's
# threat model intact:
#   1. Auth-bearing properties (-Dsonar.token=, -Dsonar.login=,
#      -Dsonar.password=). The plugin's whole point is "token
#      lives in SONAR_TOKEN env, never on argv" so allowing them
#      to slip in via extra-props would defeat it. Operators
#      route auth through `secrets:` + `token:` instead.
#   2. Blank lines and #-comments are skipped so the YAML block
#      stays readable.
# Case-insensitive match because the scanner accepts property
# names regardless of case.
extra_props_arr=()
if [ -n "${PLUGIN_EXTRA_PROPS:-}" ]; then
    while IFS= read -r line; do
        # Strip leading whitespace (POSIX-portable pattern).
        line="${line#"${line%%[![:space:]]*}"}"
        # Skip blanks + comments.
        case "$line" in
            ''|'#'*) continue ;;
        esac
        # Reject token-bearing properties.
        lower=$(printf '%s' "$line" | tr '[:upper:]' '[:lower:]')
        case "$lower" in
            -dsonar.token=*|-dsonar.login=*|-dsonar.password=*)
                key="${line%%=*}"
                key="${key#-D}"
                key="${key#-d}"
                echo "gocdnext/sonar: extra-props refuses to forward '${key}'" >&2
                echo "  auth must come via secrets: + token: input — the scanner" >&2
                echo "  reads SONAR_TOKEN from env so plaintext never lands on argv" >&2
                exit 2
                ;;
        esac
        extra_props_arr+=("$line")
    done <<< "${PLUGIN_EXTRA_PROPS}"
fi

# --- dispatch ---------------------------------------------------
echo "==> sonar mode=${MODE} host=${HOST_URL} project=${PROJECT_KEY}${PLUGIN_PULL_REQUEST_KEY:+ pr=${PLUGIN_PULL_REQUEST_KEY}}${PLUGIN_BRANCH:+ branch=${PLUGIN_BRANCH}}"

case "${MODE}" in
    maven)
        # sonar-maven-plugin auto-resolves from Maven Central via
        # the `sonar:` prefix — no project pom.xml edit needed.
        # `--no-transfer-progress` mirrors the standalone maven
        # plugin's log hygiene.
        exec mvn --batch-mode --no-transfer-progress \
            "-Dmaven.repo.local=${MAVEN_LOCAL_REPO}" \
            sonar:sonar \
            "${sonar_props[@]}" \
            "${extra_props_arr[@]}"
        ;;
    gradle)
        # Requires the project's build.gradle{,.kts} to apply
        # `org.sonarqube` plugin. The `sonar` task replaced
        # `sonarqube` in plugin v3.0+ (2022); try `sonar` first
        # — the user can override the task name via extra-props
        # if they're pinned to an older plugin version.
        if [ -x "./gradlew" ]; then
            CLI="./gradlew"
        else
            CLI="gradle"
        fi
        exec "${CLI}" --no-daemon sonar \
            "${sonar_props[@]}" \
            "${extra_props_arr[@]}"
        ;;
    scanner-cli)
        # sonar-project.properties in the repo root takes precedence
        # over `-D` flags for sonar-scanner CLI; document this in
        # plugin.yaml so operators know the resolution order.
        exec sonar-scanner \
            "${sonar_props[@]}" \
            "${extra_props_arr[@]}"
        ;;
esac
