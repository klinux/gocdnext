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

echo "==> mvn ${PLUGIN_COMMAND}"
# shellcheck disable=SC2086
exec mvn --batch-mode "${settings_arg[@]}" ${PLUGIN_COMMAND}
