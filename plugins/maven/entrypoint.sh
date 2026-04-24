#!/bin/bash
# gocdnext/maven — thin wrapper that optionally synthesises a
# settings.xml from PLUGIN_NEXUS_* env so operators don't have to
# check in credentials. See Dockerfile for the full contract.

set -euo pipefail

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/maven: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: verify" >&2
    echo "    command: clean package -DskipTests" >&2
    echo "    command: deploy -Pprod" >&2
    exit 2
fi

cd /workspace
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
export MAVEN_LOCAL_REPO="${MAVEN_LOCAL_REPO:-/workspace/.m2-repo}"
mkdir -p "${MAVEN_LOCAL_REPO}"
local_repo_arg=("-Dmaven.repo.local=${MAVEN_LOCAL_REPO}")

settings_arg=()
if [ -n "${PLUGIN_SETTINGS:-}" ]; then
    # Operator-provided settings.xml wins — they probably
    # already encode the repositories, mirrors, and policies
    # they care about.
    settings_arg+=("--settings" "/workspace/${PLUGIN_SETTINGS}")
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

echo "==> mvn ${PLUGIN_COMMAND} (repo.local=${MAVEN_LOCAL_REPO})"
# shellcheck disable=SC2086
exec mvn --batch-mode "${local_repo_arg[@]}" "${settings_arg[@]}" ${PLUGIN_COMMAND}
