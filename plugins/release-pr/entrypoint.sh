#!/bin/bash
# gocdnext/release-pr — open (or update) a curated release PR. See
# Dockerfile for the full contract.
#
# The flow this serves: a release manager runs this (a manual
# pipeline). It bumps a VERSION file on a release/<ver> branch — based
# on the BASE branch, not whatever ref the run was triggered on — and
# opens a PR for peer sign-off. Merging that PR (which touches VERSION)
# is what a downstream `on: push, paths: [VERSION]` pipeline keys on to
# cut the git tag. Curation + approval live in git/GitHub; this plugin
# only PREPARES the PR, it never deploys.
#
# Version selection is the caller's job (typically a semver-bump step
# upstream): pass the computed version in. "The release is everything
# on the base up to the cut" — to hold a merged change back, revert it
# on the base branch, not via cherry-pick here.
set -euo pipefail

fail() { echo "gocdnext/release-pr: $1" >&2; exit 2; }

[ -n "${PLUGIN_VERSION:-}" ] || fail "version is required (typically \${{ needs.bump.outputs.next }})"
[ -n "${PLUGIN_TOKEN:-}" ]   || fail "token is required (pipe via secrets:)"

tag_prefix="${PLUGIN_TAG_PREFIX:-v}"
base="${PLUGIN_BASE:-main}"
version_file="${PLUGIN_VERSION_FILE:-VERSION}"
label="${PLUGIN_LABEL:-release}"
branch_prefix="${PLUGIN_BRANCH_PREFIX:-release/}"
remote="${PLUGIN_REMOTE:-origin}"
username="${PLUGIN_USERNAME:-x-access-token}"

# ── charset guards: keep values free of whitespace, newlines and
# shell/ref/option metacharacters. Stops output-injection into
# $GOCDNEXT_OUTPUT_FILE and odd refnames / gh option edge cases.
[[ "${tag_prefix}"    =~ ^[A-Za-z0-9._/-]*$ ]] || fail "tag_prefix must match [A-Za-z0-9._/-]*"
[[ "${branch_prefix}" =~ ^[A-Za-z0-9._/-]*$ ]] || fail "branch_prefix must match [A-Za-z0-9._/-]*"
[[ "${base}"     =~ ^[A-Za-z0-9._/-]+$ ]]      || fail "base must match [A-Za-z0-9._/-]+"
[[ "${username}" =~ ^[A-Za-z0-9._/-]+$ ]]      || fail "username must match [A-Za-z0-9._/-]+"
[[ "${label}"    =~ ^[A-Za-z0-9._/\ -]+$ ]]    || fail "label has forbidden characters"
if [ -n "${PLUGIN_REPO:-}" ]; then
    [[ "${PLUGIN_REPO}" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || fail "repo must be owner/name"
fi

# Normalize version: accept both bare (1.3.0) and prefixed (v1.3.0 —
# what semver-bump with prefix:v emits) by stripping a leading
# tag_prefix, then validate SemVer so a newline/metachar can't reach a
# refname or the output file.
version="${PLUGIN_VERSION#"${tag_prefix}"}"
[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] \
    || fail "version must be SemVer major.minor.patch[-pre] (prefix optional) — got: ${PLUGIN_VERSION}"

release_name="${tag_prefix}${version}"
branch="${branch_prefix}${release_name}"
title="${PLUGIN_TITLE:-Release ${release_name}}"

# Defence in depth past the charset regex: git's own ref grammar
# rejects shapes the regex still lets through (e.g. "main..x", a
# trailing "/", "@{") — fail here with a clear message instead of a
# cryptic git error mid-push.
git check-ref-format --branch "${base}" >/dev/null 2>&1 \
    || fail "base is not a valid branch name: ${base}"
git check-ref-format "refs/heads/${branch}" >/dev/null 2>&1 \
    || fail "computed release branch is not a valid ref: ${branch}"

# ── path guards: workspace-relative, no absolute, no parent traversal.
# A notes_file like /proc/self/environ would otherwise let gh read the
# token into the PR body; version_file could write outside the repo.
for spec in "version_file=${version_file}" "notes_file=${PLUGIN_NOTES_FILE:-}"; do
    pname="${spec%%=*}"; pval="${spec#*=}"
    [ -z "${pval}" ] && continue
    case "${pval}" in
        /*)               fail "${pname} must be workspace-relative (no leading /) — got: ${pval}" ;;
        ../*|*/../*|*/..)  fail "${pname} must not traverse outside the workspace (no '..') — got: ${pval}" ;;
    esac
done

git config --global --add safe.directory '*' 2>/dev/null || true
git config --global user.email "${GIT_AUTHOR_EMAIL:-release-bot@gocdnext.local}"
git config --global user.name "${GIT_AUTHOR_NAME:-gocdnext-release-bot}"

# Auth via GIT_ASKPASS so the token never lands on argv (/proc/<pid>/
# cmdline, ps) nor in git's URL-embedding error output. The fetch/push
# URLs stay clean.
ASKPASS="$(mktemp)"; trap 'rm -f "${ASKPASS}"' EXIT
cat > "${ASKPASS}" <<'EOF'
#!/bin/sh
case "$1" in
    Username*) echo "${GIT_PUSH_USERNAME}" ;;
    *)         echo "${GIT_PUSH_TOKEN}" ;;
esac
EOF
chmod 700 "${ASKPASS}"
export GIT_ASKPASS="${ASKPASS}" GIT_TERMINAL_PROMPT=0
export GIT_PUSH_USERNAME="${username}" GIT_PUSH_TOKEN="${PLUGIN_TOKEN}"

# Clean origin URL — strip any embedded clone-token creds the agent
# rode in on (read-only / short-lived; pushes use PLUGIN_TOKEN).
origin_url=$(git remote get-url "${remote}")
case "${origin_url}" in
    https://*) ;;
    *) fail "remote ${remote} is not https (${origin_url}); SSH unsupported in v1" ;;
esac
hostpath="${origin_url#https://}"
hostpath="${hostpath#*@}"
clean_url="https://${hostpath}"

# Branch from BASE, not the current checkout — the release PR must
# carry only the VERSION bump on top of base, regardless of which ref
# the manual run was triggered on.
echo "==> fetching ${base}, branching ${branch}"
git fetch "${clean_url}" "${base}"
git checkout -B "${branch}" FETCH_HEAD

printf '%s\n' "${version}" > "${version_file}"
git add -- "${version_file}"
# Re-run guard: nothing to commit if VERSION already equals ${version}.
if ! git diff --cached --quiet; then
    git commit -m "release ${release_name}"
fi

# Force-with-lease with an EXPLICIT expected value. Pushing to a URL
# (not a configured remote) leaves no reliable remote-tracking ref, so
# a bare --force-with-lease has nothing to compare and a re-run aborts
# with "stale info". Read the remote tip with ls-remote and lease
# against exactly that: empty when the branch is new (safe create),
# the current sha when it exists. A concurrent cut that moved the
# branch fails the push loud instead of silently clobbering its PR.
remote_ref="refs/heads/${branch}"
remote_sha="$(git ls-remote --heads "${clean_url}" "${branch}" | awk 'NR==1{print $1}')"
lease=(--force-with-lease="${remote_ref}:${remote_sha}")
echo "==> git push ${branch} (force-with-lease${remote_sha:+ @ ${remote_sha}})"
git push "${lease[@]}" "${clean_url}" "HEAD:${remote_ref}" \
    || fail "push to ${branch} failed (lease violation? a concurrent release cut may have moved it) — see output above"

# Open or update the PR. gh authenticates via GH_TOKEN (kept off argv).
export GH_TOKEN="${PLUGIN_TOKEN}"
repo_args=(); [ -n "${PLUGIN_REPO:-}" ] && repo_args+=(--repo "${PLUGIN_REPO}")
body_args=()
if [ -n "${PLUGIN_NOTES_FILE:-}" ] && [ -f "${PLUGIN_NOTES_FILE}" ]; then
    body_args+=(--body-file "${PLUGIN_NOTES_FILE}")
else
    body_args+=(--body "Automated release PR for ${release_name}.")
fi

if gh pr view "${branch}" "${repo_args[@]}" >/dev/null 2>&1; then
    echo "==> updating existing PR for ${branch}"
    gh pr edit "${branch}" "${repo_args[@]}" --title "${title}" "${body_args[@]}" --add-label "${label}"
else
    echo "==> creating PR ${branch} -> ${base}"
    gh pr create "${repo_args[@]}" --base "${base}" --head "${branch}" --title "${title}" "${body_args[@]}" --label "${label}"
fi

# Surface the PR URL + version as outputs for downstream jobs.
pr_url=$(gh pr view "${branch}" "${repo_args[@]}" --json url --jq .url 2>/dev/null || true)
if [ -n "${GOCDNEXT_OUTPUT_FILE:-}" ]; then
    {
        echo "pr_url=${pr_url}"
        echo "version=${version}"
        echo "release_tag=${release_name}"
    } >> "${GOCDNEXT_OUTPUT_FILE}"
fi
echo "==> release PR ready: ${pr_url:-<created>}"
