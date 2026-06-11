#!/bin/sh
# gocdnext/dotnet entrypoint — thin shim around `dotnet` so
# pipelines don't hand-roll SDK setup in every job.
#
# Inputs (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  dotnet subcommand + args, word-split.
#   PLUGIN_WORKING_DIR  (optional)  relative path under /workspace.
#   PLUGIN_SDK          (optional)  "8" or "10" — pin the SDK major when
#                                   the repo has NO global.json. A repo
#                                   that ships global.json already pins;
#                                   setting both is two sources of truth
#                                   and fails loud (exit 2) instead of
#                                   letting them drift silently.
#
# Exits with the dotnet CLI's own exit code.

set -eu

if [ -z "${PLUGIN_COMMAND:-}" ]; then
    echo "gocdnext/dotnet: PLUGIN_COMMAND is required" >&2
    echo "  examples:" >&2
    echo "    command: test -c Release" >&2
    echo "    command: build" >&2
    echo "    command: publish -c Release -o out" >&2
    exit 2
fi

# Capture the workspace root BEFORE any cd. Plugin-written SDK
# pins always land HERE (not at working-dir) so a job has exactly
# ONE canonical pin location — two tasks with different
# working-dirs can't each write their own pin into sibling
# subdirs and silently disagree. The muxer finds a root pin from
# any working-dir via its own upward walk.
workspace_root=$(pwd)

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

# Git 2.35+ "dubious ownership" — same workaround as every other
# plugin that hits `git` on the host-cloned workspace (SourceLink
# and MinVer-style versioning read git metadata during build).
git config --global --add safe.directory '*' 2>/dev/null || true

# SDK pin. The dotnet muxer's native selector is global.json, so
# that's the mechanism we drive — no env var exists for SDK
# selection within a single DOTNET_ROOT. Resolution:
#   - repo ships global.json + no input  → muxer handles it, we
#     stay out of the way.
#   - repo ships global.json + sdk input → exit 2. Two pins
#     drifting apart is exactly the silent-version-skew bug this
#     input exists to avoid.
#   - no global.json + sdk input         → resolve the exact
#     installed version for the requested major from --list-sdks
#     and write a global.json with rollForward latestPatch.
#   - neither                            → muxer default (the
#     newest installed SDK, currently 10).
#
# The conflict check MUST mirror the muxer's lookup: it walks UP
# from cwd to the filesystem root and the closest global.json
# wins. Testing only `pwd` would miss a repo-root global.json
# when `working-dir:` points at a subdir — and the file we'd
# write into the subdir would SHADOW the repo's pin, silently
# creating exactly the two-pin drift this guard exists to stop.
find_global_json() {
    d=$(pwd)
    while :; do
        if [ -f "${d}/global.json" ]; then
            printf '%s' "${d}/global.json"
            return 0
        fi
        if [ "${d}" = "/" ]; then
            return 1
        fi
        d=$(dirname "${d}")
    done
}

if [ -n "${PLUGIN_SDK:-}" ]; then
    case "${PLUGIN_SDK}" in
        8|10) ;;
        *)
            echo "gocdnext/dotnet: sdk accepts 8 or 10 (got '${PLUGIN_SDK}')" >&2
            echo "  installed SDKs:" >&2
            dotnet --list-sdks >&2
            exit 2
            ;;
    esac
    resolved=$(dotnet --list-sdks | awk -v major="${PLUGIN_SDK}." 'index($1, major) == 1 { v = $1 } END { print v }')
    if [ -z "${resolved}" ]; then
        echo "gocdnext/dotnet: no installed SDK matches major ${PLUGIN_SDK}" >&2
        dotnet --list-sdks >&2
        exit 2
    fi
    # The pin we write carries a marker key so re-entry can tell
    # OUR file from the repo's. global.json tolerates unknown
    # top-level properties (the muxer only reads "sdk" /
    # "msbuild-sdks"), so the marker is invisible to the SDK.
    # Without it, a repo global.json that HAPPENS to be
    # byte-identical to our output would pass the idempotency
    # check today and explode with exit 2 on the next SDK patch
    # of this image — a delayed failure with no repo change to
    # explain it. Marker = provenance, not just content.
    marker='"_gocdnext": "sdk pin written by the dotnet plugin — safe to delete"'
    desired=$(printf '{\n  %s,\n  "sdk": { "version": "%s", "rollForward": "latestPatch" }\n}\n' "${marker}" "${resolved}")
    if existing=$(find_global_json); then
        if [ "$(cat "${existing}")" = "${desired}" ]; then
            # Marker present AND byte-identical: our own pin from an
            # earlier task of this job (tasks share the workspace).
            echo "==> SDK ${resolved} already pinned via ${existing}"
        elif grep -q '"_gocdnext"' "${existing}"; then
            # Plugin-written, but different content: two tasks of
            # this job pin DIFFERENT sdk majors, or a stale pin
            # from another plugin image version. Either way the
            # inputs contradict each other — fail loud.
            echo "gocdnext/dotnet: conflicting plugin pin at ${existing}" >&2
            echo "  another task of this job pinned a different SDK (or a different plugin version wrote it)" >&2
            echo "  align the 'sdk:' inputs across tasks — one job, one SDK pin" >&2
            exit 2
        else
            echo "gocdnext/dotnet: sdk input is set but the repo already pins via ${existing}" >&2
            echo "  (the dotnet muxer resolves global.json walking up from working-dir — the closest file wins)" >&2
            echo "  remove the 'sdk:' input (repo file wins) or delete the global.json (input wins)" >&2
            exit 2
        fi
    else
        # Write at the WORKSPACE ROOT, not the working-dir. The
        # detection above walks up from working-dir, so a root pin
        # is seen by every later task regardless of its
        # working-dir — but a working-dir pin would be invisible
        # to a later task running closer to the root (walk-up
        # never descends), letting two plugin pins coexist and
        # disagree. Root placement makes "one job, one SDK pin"
        # structural instead of best-effort.
        printf '%s\n' "${desired}" > "${workspace_root}/global.json"
        echo "==> pinned SDK ${resolved} via ${workspace_root}/global.json"
    fi
fi

# Redirect the NuGet package cache into the workspace so the
# platform's `cache:` block can tar it. Restore is usually the
# dominant cost of a cold .NET build; the base image default sits
# outside the workspace at /root/.nuget/packages.
export NUGET_PACKAGES="${NUGET_PACKAGES:-$(pwd)/.nuget-packages}"
mkdir -p "${NUGET_PACKAGES}"

# Testcontainers auto-config — ONLY when the host's
# /var/run/docker.sock is mounted into this container (docker-engine
# path; the agent wires `host.docker.internal` on the run's bridge).
# DO NOT trigger on DOCKER_HOST: in the Kubernetes DinD path
# Testcontainers for .NET auto-detects via DOCKER_HOST natively and
# a socket-override pointing at a non-existent path would break it.
# Same rationale + shape as the go plugin.
if [ -S /var/run/docker.sock ]; then
    export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="${TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE:-/var/run/docker.sock}"
    export TESTCONTAINERS_HOST_OVERRIDE="${TESTCONTAINERS_HOST_OVERRIDE:-host.docker.internal}"
fi

# Banner so the log shows what's about to run — a cold
# `dotnet restore` can be silent for many seconds, and a silent
# log reads like the job is hung.
echo "==> dotnet ${PLUGIN_COMMAND}"

# Word-split intentionally: `test -c Release` is three args. If an
# operator needs whitespace inside a single arg, drop back to plain
# `script:`; this plugin optimises the 90% case.
# shellcheck disable=SC2086
exec dotnet ${PLUGIN_COMMAND}
