#!/bin/bash
# select-jdk.sh — set JAVA_HOME + PATH for the JDK the operator
# asked for via the `jdk:` input on their plugin step (mapped to
# the PLUGIN_JDK env var by the agent).
#
# Source this from a downstream plugin's entrypoint:
#
#     . /usr/local/bin/select-jdk.sh
#
# Accepted values: 11, 17, 21, 25. Default 21 (current
# most-deployed LTS in 2026). Unknown values fail loud with
# exit 2 — a typo (`jdk: "211"`) MUST NOT silently fall through
# to a different JDK; the operator's intent was explicit.
#
# PATH manipulation is idempotent: re-sourcing this script with
# the same value (or a different one) doesn't accrete duplicate
# entries — the previous JDK's bin dir is stripped before the
# new one is prepended.

# Strip every /opt/java/jdk-N/bin entry from PATH so re-sourcing
# this script doesn't pile up. Bracket-character class matches a
# single digit; runs in the regex-extended form sed -E.
_PATH_STRIPPED=$(printf '%s' "${PATH:-}" | sed -E 's#(^|:)/opt/java/jdk-[0-9]+/bin(:|$)#\1#g; s#^:##; s#:$##; s#::#:#g')

case "${PLUGIN_JDK:-21}" in
    11|17|21|25)
        _JDK="${PLUGIN_JDK:-21}"
        if [ ! -x "/opt/java/jdk-${_JDK}/bin/java" ]; then
            echo "select-jdk: /opt/java/jdk-${_JDK} not present in this image" >&2
            exit 2
        fi
        export JAVA_HOME="/opt/java/jdk-${_JDK}"
        export PATH="${JAVA_HOME}/bin:${_PATH_STRIPPED}"
        echo "gocdnext: JDK ${_JDK} selected (JAVA_HOME=${JAVA_HOME})"
        unset _JDK _PATH_STRIPPED
        ;;
    *)
        echo "gocdnext: jdk: must be 11, 17, 21, or 25 (got '${PLUGIN_JDK}')" >&2
        exit 2
        ;;
esac
