#!/bin/bash
# gocdnext/semver-bump — compute next SemVer from Conventional
# Commits since the prior tag. See Dockerfile for the contract.

set -euo pipefail

PREFIX="${PLUGIN_PREFIX-v}"
INITIAL="${PLUGIN_INITIAL:-0.1.0}"
PRE_RELEASE="${PLUGIN_PRE_RELEASE:-}"
FORCE_KIND="${PLUGIN_FORCE_KIND:-}"
OUTPUT="${PLUGIN_OUTPUT:-.gocdnext/semver.env}"
PRIOR_TAG_OVERRIDE="${PLUGIN_PRIOR_TAG:-}"

# --- validate inputs --------------------------------------------------

# Prefix lands LITERALLY in the shell-sourceable output file (NEXT='<prefix>X.Y.Z')
# AND in the `git describe --match` glob. A value like `v'; rm -rf /; #` would
# both inject shell into the consumer that `source`s the file AND mangle the
# glob. Restrict to the smallest charset that covers every legitimate use
# (`v`, ``, `release-`, `api-v`, `web/`): letters, digits, dot, hyphen, slash,
# underscore. No quotes, no spaces, no shell metas.
if ! [[ "${PREFIX}" =~ ^[A-Za-z0-9._/-]*$ ]]; then
    echo "gocdnext/semver-bump: PLUGIN_PREFIX must match [A-Za-z0-9._/-]* — got: ${PREFIX}" >&2
    exit 2
fi

# Output path must stay inside the workspace. Reject absolute paths
# (`/etc/passwd`) and parent traversal (`../escape`) so a malicious or
# typo'd input can't write outside the job's working directory. Empty
# is already rejected by the default-fallback.
if [ -z "${OUTPUT}" ]; then
    echo "gocdnext/semver-bump: PLUGIN_OUTPUT must not be empty" >&2
    exit 2
fi
case "${OUTPUT}" in
    /*)
        echo "gocdnext/semver-bump: PLUGIN_OUTPUT must be a workspace-relative path (no leading /) — got: ${OUTPUT}" >&2
        exit 2
        ;;
esac
case "${OUTPUT}" in
    ../*|*/../*|*/..)
        echo "gocdnext/semver-bump: PLUGIN_OUTPUT must not traverse outside the workspace (no '..' segments) — got: ${OUTPUT}" >&2
        exit 2
        ;;
esac

# SemVer regex (loose — captures major.minor.patch, ignores existing
# pre-release suffix on the current tag when computing the next).
# Reject anything that doesn't fit so a bad PLUGIN_INITIAL surfaces
# at apply rather than producing a garbage NEXT downstream.
if ! [[ "${INITIAL}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
    echo "gocdnext/semver-bump: PLUGIN_INITIAL must be SemVer (e.g. 0.1.0) — got: ${INITIAL}" >&2
    exit 2
fi

case "${FORCE_KIND}" in
    ""|major|minor|patch) ;;
    *)
        echo "gocdnext/semver-bump: PLUGIN_FORCE_KIND must be one of: major, minor, patch (or empty) — got: ${FORCE_KIND}" >&2
        exit 2
        ;;
esac

# Pre-release suffix must be SemVer-conformant identifier set
# (alphanumeric + hyphen + dot separators). Reject shell metas + spaces
# so a typo doesn't end up in a tag name.
if [ -n "${PRE_RELEASE}" ]; then
    if ! [[ "${PRE_RELEASE}" =~ ^[A-Za-z0-9.-]+$ ]]; then
        echo "gocdnext/semver-bump: PLUGIN_PRE_RELEASE must match [A-Za-z0-9.-]+ — got: ${PRE_RELEASE}" >&2
        exit 2
    fi
fi

# --- find prior tag ---------------------------------------------------

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "gocdnext/semver-bump: not inside a git repository (cwd=$(pwd))" >&2
    exit 3
fi

# When the operator pinned PRIOR_TAG_OVERRIDE, use it verbatim — must
# itself be a SemVer-shaped tag (validated downstream when we parse
# `current_bare`). Otherwise `git describe` finds the nearest reachable
# tag matching the prefix glob. `--abbrev=0` strips the "+ N commits"
# suffix so we get the pure tag name.
#
# Glob is `<prefix>[0-9]*` (NOT `<prefix>*`) to require a digit
# immediately after the prefix. This filters out non-SemVer tags
# sharing the prefix — `vfoo`, `vnext`, `vendor-*`, etc. — that
# would otherwise confuse the SemVer parser below. Tags like
# `v1.2.3-rc1` still match because `[0-9]*` is greedy and consumes
# everything from the first digit. Empty PREFIX → glob becomes
# `[0-9]*`, scanning bare SemVer (1.2.3) tags only.
prior_tag=""
if [ -n "${PRIOR_TAG_OVERRIDE}" ]; then
    prior_tag="${PRIOR_TAG_OVERRIDE}"
    if ! git rev-parse --verify "${prior_tag}^{commit}" >/dev/null 2>&1; then
        echo "gocdnext/semver-bump: PLUGIN_PRIOR_TAG ref does not exist: ${prior_tag}" >&2
        exit 3
    fi
else
    match_glob="${PREFIX}[0-9]*"
    # `|| true` because git describe exits non-zero on "no tag
    # reachable" — that's a legit "first release" path, not an
    # error.
    prior_tag=$(git describe --tags --match "${match_glob}" --abbrev=0 2>/dev/null || true)
fi

# --- determine current version ----------------------------------------

# Strip the prefix to leave bare SemVer. When no prior tag exists
# we synthesise a current from the initial input, so the bump
# logic below applies uniformly (the kind-detection then decides
# whether to emit the initial as-is or bump it further).
if [ -z "${prior_tag}" ]; then
    current_bare="${INITIAL}"
    prev_sha=""
else
    current_bare="${prior_tag#${PREFIX}}"
fi

# Validate the parsed current (in case PLUGIN_PREFIX didn't match
# what the actual tag uses, leaving us with garbage). Better to
# fail loud than to produce v(.+1) style noise.
if ! [[ "${current_bare}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)(-[A-Za-z0-9.-]+)?$ ]]; then
    echo "gocdnext/semver-bump: parsed current version is not SemVer: ${current_bare} (from tag ${prior_tag:-<none>})" >&2
    echo "  hint: check PLUGIN_PREFIX (currently '${PREFIX}') matches the tag scheme" >&2
    exit 3
fi
cur_major="${BASH_REMATCH[1]}"
cur_minor="${BASH_REMATCH[2]}"
cur_patch="${BASH_REMATCH[3]}"

if [ -n "${prior_tag}" ]; then
    prev_sha=$(git rev-list -n 1 "${prior_tag}" 2>/dev/null || true)
fi

# --- detect bump kind from commits since prior_tag --------------------

kind=""
if [ -n "${FORCE_KIND}" ]; then
    kind="${FORCE_KIND}"
else
    # When there's no prior tag, the "first release" emits the
    # initial as-is — no scan needed, no bump applied.
    if [ -z "${prior_tag}" ]; then
        kind="initial"
    else
        # Scan commit subjects + bodies between prior_tag and HEAD.
        # Conventional Commits:
        #   - "feat!:" or "fix!:" or "refactor!:" etc. → major
        #   - "BREAKING CHANGE:" anywhere in the body → major
        #   - "feat" / "feat(scope)" → minor
        #   - "fix" / "fix(scope)" / "perf" / others → patch
        #
        # Default fallback: patch. Conservative — if nothing in
        # the range looks intentional, treat the cycle as a patch
        # rather than refusing to bump (the alternative — kind=none —
        # is reachable only via "no commits at all" which is
        # already its own branch below).
        range="${prior_tag}..HEAD"
        commit_count=$(git rev-list --count "${range}" 2>/dev/null || echo 0)

        if [ "${commit_count}" -eq 0 ]; then
            kind="none"
        else
            # %B prints the full message (subject + body) per
            # commit. Git always emits a trailing newline after
            # each %B, so subject lines stay on their own lines —
            # the regex anchors below correctly attribute a
            # "feat!:" pattern to a subject, not to a body
            # paragraph that happens to start that way. LC_ALL=C
            # keeps grep predictable against unicode commit
            # messages.
            log=$(LC_ALL=C git log "${range}" --format='%B' 2>/dev/null || true)

            # Use here-strings (<<<) instead of `echo "$log" | grep`
            # because `set -o pipefail` + `grep -q` short-circuits
            # on the first match and triggers SIGPIPE on the
            # upstream `echo`, making the pipeline status non-zero
            # even on a successful match. The `if` would then take
            # the else branch and miss a feat:/major commit hidden
            # inside a large log. Herestrings feed grep via stdin
            # without a pipe — no SIGPIPE possible.
            #
            # Major: explicit "!" before the colon on the subject OR
            # "BREAKING CHANGE:" anywhere in a body. The leading-
            # newline anchor (^|\n) on BREAKING CHANGE keeps it from
            # matching a string containing those words mid-line.
            if grep -qE '^[a-z]+(\([^)]+\))?!:' <<< "${log}" \
                || grep -qE '(^|[[:space:]])BREAKING CHANGE:' <<< "${log}"; then
                kind="major"
            elif grep -qE '^feat(\([^)]+\))?:' <<< "${log}"; then
                kind="minor"
            else
                kind="patch"
            fi
        fi
    fi
fi

# --- compute next version ---------------------------------------------

case "${kind}" in
    major)
        next_major=$((cur_major + 1))
        next_minor=0
        next_patch=0
        ;;
    minor)
        next_major="${cur_major}"
        next_minor=$((cur_minor + 1))
        next_patch=0
        ;;
    patch)
        next_major="${cur_major}"
        next_minor="${cur_minor}"
        next_patch=$((cur_patch + 1))
        ;;
    initial)
        # First release — emit the initial as-is.
        next_major="${cur_major}"
        next_minor="${cur_minor}"
        next_patch="${cur_patch}"
        ;;
    none)
        # No commits since the prior tag — NEXT == CURRENT, the
        # downstream branches on KIND=none to skip the cut.
        next_major="${cur_major}"
        next_minor="${cur_minor}"
        next_patch="${cur_patch}"
        ;;
esac

next_bare="${next_major}.${next_minor}.${next_patch}"
if [ -n "${PRE_RELEASE}" ] && [ "${kind}" != "none" ]; then
    next_bare="${next_bare}-${PRE_RELEASE}"
fi
next_tag="${PREFIX}${next_bare}"
current_tag="${PREFIX}${current_bare}"

# --- write output file ------------------------------------------------

output_dir=$(dirname "${OUTPUT}")
if [ "${output_dir}" != "." ]; then
    mkdir -p "${output_dir}"
fi

# Shell-sourceable output. Values are quoted with single quotes
# (POSIX-safe; we know no single quotes appear in the parsed
# SemVer / sha / kind set, so no escaping needed). The downstream
# `source` reads NEXT / CURRENT / KIND / PREV_SHA as env.
{
    echo "# Generated by gocdnext/semver-bump — do not edit."
    echo "NEXT='${next_tag}'"
    echo "CURRENT='${current_tag}'"
    echo "KIND='${kind}'"
    echo "PREV_SHA='${prev_sha}'"
} > "${OUTPUT}"

# --- echo to job log so operators see it in the run detail ------------

echo "==> semver-bump:"
echo "      CURRENT = ${current_tag}"
echo "      NEXT    = ${next_tag}"
echo "      KIND    = ${kind}"
if [ -n "${prev_sha}" ]; then
    echo "      PREV_SHA= ${prev_sha}"
fi
echo "      written to: ${OUTPUT}"
