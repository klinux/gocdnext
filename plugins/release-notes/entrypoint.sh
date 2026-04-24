#!/bin/bash
# gocdnext/release-notes — generate a changelog between two git
# refs. See Dockerfile for the full contract.

set -euo pipefail

cd /workspace

# Same dubious-ownership opt-in as the other git plugins.
git config --global --add safe.directory '*' 2>/dev/null || true

to="${PLUGIN_TO:-HEAD}"
output="${PLUGIN_OUTPUT:-RELEASE_NOTES.md}"
format="${PLUGIN_FORMAT:-plain}"

from="${PLUGIN_FROM:-}"
if [ -z "${from}" ]; then
    # Fall back to the nearest tag reachable from the target ref.
    # `describe --tags --abbrev=0` returns the tag name itself
    # (no "-N-gHASH" suffix), which is what we want as the
    # exclusive lower bound. When no tag exists git exits with
    # "fatal: No names found" — we catch that and walk back to
    # the root commit instead, so a first release still works.
    from=$(git describe --tags --abbrev=0 "${to}" 2>/dev/null || true)
    if [ -z "${from}" ]; then
        from=$(git rev-list --max-parents=0 "${to}" | head -n 1)
        echo "==> no previous tag; walking from repo root ${from}"
    else
        echo "==> previous tag: ${from}"
    fi
fi

range="${from}..${to}"
echo "==> range ${range} → ${output}"

# Pull commits as tab-separated records so the downstream
# formatters can split without regex gymnastics.
raw=$(git log --pretty=format:'%h%x09%s%x09%an' "${range}" || true)

dest="/workspace/${output#/}"
mkdir -p "$(dirname "${dest}")"
: >"${dest}"

if [ -n "${PLUGIN_HEADING:-}" ]; then
    printf '%s\n\n' "${PLUGIN_HEADING}" >>"${dest}"
fi

if [ -z "${raw}" ]; then
    printf '%s\n' "_No commits in this range._" >>"${dest}"
    echo "==> 0 commits"
    exit 0
fi

count=0
case "${format}" in
    plain)
        while IFS=$'\t' read -r sha subject author; do
            printf -- '- %s %s (%s)\n' "${sha}" "${subject}" "${author}" >>"${dest}"
            count=$((count + 1))
        done <<<"${raw}"
        ;;

    conventional)
        declare -A bucket_features=()
        declare -A bucket_fixes=()
        declare -A bucket_chores=()
        declare -A bucket_other=()
        # Readable insertion order per bucket via indexed arrays:
        feats=()
        fixes=()
        chores=()
        others=()

        while IFS=$'\t' read -r sha subject author; do
            line=$(printf -- '- %s %s (%s)' "${sha}" "${subject}" "${author}")
            # Match the Conventional Commits prefix at the start
            # of the subject. Accept optional scope + bang.
            if [[ "${subject}" =~ ^feat(\(.+\))?!?: ]]; then
                feats+=("${line}")
            elif [[ "${subject}" =~ ^fix(\(.+\))?!?: ]]; then
                fixes+=("${line}")
            elif [[ "${subject}" =~ ^(chore|build|ci|docs|refactor|style|test|perf)(\(.+\))?!?: ]]; then
                chores+=("${line}")
            else
                others+=("${line}")
            fi
            count=$((count + 1))
        done <<<"${raw}"

        emit_bucket() {
            local title="$1"; shift
            [ $# -eq 0 ] && return
            printf '\n### %s\n\n' "${title}" >>"${dest}"
            for line in "$@"; do
                printf '%s\n' "${line}" >>"${dest}"
            done
        }
        emit_bucket "Features" "${feats[@]}"
        emit_bucket "Bug Fixes" "${fixes[@]}"
        emit_bucket "Chores" "${chores[@]}"
        emit_bucket "Other" "${others[@]}"
        ;;

    *)
        echo "gocdnext/release-notes: unknown format '${format}' (accepted: plain, conventional)" >&2
        exit 2
        ;;
esac

echo "==> ${count} commit(s) written to ${output}"
