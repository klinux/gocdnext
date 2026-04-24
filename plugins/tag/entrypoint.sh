#!/bin/bash
# gocdnext/tag — create + push a git tag. See Dockerfile for the
# full contract.

set -euo pipefail

if [ -z "${PLUGIN_NAME:-}" ]; then
    echo "gocdnext/tag: name is required (e.g. v1.2.3)" >&2
    exit 2
fi
if [ -z "${PLUGIN_TOKEN:-}" ]; then
    echo "gocdnext/tag: token is required (pipe via secrets:)" >&2
    exit 2
fi

name="${PLUGIN_NAME}"
revision="${PLUGIN_REVISION:-HEAD}"
remote="${PLUGIN_REMOTE:-origin}"
username="${PLUGIN_USERNAME:-git}"

cd /workspace

# The host-cloned workspace is owned by the agent's UID while
# the container runs as root; git 2.35+ refuses to operate on
# repos with mismatched ownership without this opt-in.
git config --global --add safe.directory '*' 2>/dev/null || true

# The agent stamps the email/name from the run's triggered_by
# when it's available; fall back to gocdnext-generic identity
# so `git tag -a` doesn't abort with "please tell me who you
# are". These get written into the tag object, not commits.
git config --global user.email "${GIT_AUTHOR_EMAIL:-ci@gocdnext.local}"
git config --global user.name "${GIT_AUTHOR_NAME:-gocdnext}"

force_args=()
if [ "${PLUGIN_FORCE:-false}" = "true" ]; then
    force_args+=(--force)
fi

# Annotated vs. lightweight: annotated carries metadata
# (tagger + date + message), which is what GitHub Releases +
# semver tooling expect. Lightweight tags are just a ref to a
# commit — fine for "latest" floating markers but poor choice
# for semver release tags.
if [ -n "${PLUGIN_MESSAGE:-}" ]; then
    echo "==> git tag -a ${name} ${revision}"
    git tag "${force_args[@]}" -a "${name}" "${revision}" -m "${PLUGIN_MESSAGE}"
else
    echo "==> git tag ${name} ${revision} (lightweight)"
    git tag "${force_args[@]}" "${name}" "${revision}"
fi

# Resolve the remote URL and rewrite to include the token. Doing
# this per-invocation (vs. a persistent credential helper) keeps
# the secret out of any config file that might get checked in.
remote_url=$(git remote get-url "${remote}")
case "${remote_url}" in
    https://*)
        stripped="${remote_url#https://}"
        auth_url="https://${username}:${PLUGIN_TOKEN}@${stripped}"
        ;;
    http://*)
        stripped="${remote_url#http://}"
        auth_url="http://${username}:${PLUGIN_TOKEN}@${stripped}"
        ;;
    *)
        echo "gocdnext/tag: remote ${remote} is not https (${remote_url}); SSH auth isn't supported in v1" >&2
        exit 2
        ;;
esac

echo "==> git push ${remote} ${name}"
# Push the single ref rather than `--tags`: `--tags` pushes every
# tag in the local repo, which might include stale locals the
# agent cloned from a shallow checkout. Named push is precise.
git push "${force_args[@]}" "${auth_url}" "refs/tags/${name}"
echo "==> pushed"
