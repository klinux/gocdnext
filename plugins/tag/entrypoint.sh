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

# Auth via GIT_ASKPASS so the token never lands on argv (/proc/<pid>/
# cmdline, ps) or in git's URL-embedding error output.
ASKPASS="$(mktemp)"
trap 'rm -f "${ASKPASS}"' EXIT
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

# Push to the origin URL with any embedded clone-token creds STRIPPED.
# The agent clones private repos as https://x-access-token:CLONE@host/…
# so a naive `https://user:token@${remote_url#https://}` would double
# the creds — git then reads the second token as a port ("Port number
# was not a decimal number"). Strip the `user:token@` and let
# GIT_ASKPASS supply the push credential.
remote_url=$(git remote get-url "${remote}")
case "${remote_url}" in
    https://*) scheme="https://" ;;
    http://*)  scheme="http://" ;;
    *)
        echo "gocdnext/tag: remote ${remote} is not https (${remote_url}); SSH auth isn't supported in v1" >&2
        exit 2
        ;;
esac
hostpath="${remote_url#"${scheme}"}"
hostpath="${hostpath#*@}"
clean_url="${scheme}${hostpath}"

echo "==> git push ${remote} refs/tags/${name}"
# Push the single ref rather than `--tags`: `--tags` pushes every
# tag in the local repo, which might include stale locals the
# agent cloned from a shallow checkout. Named push is precise.
git push "${force_args[@]}" "${clean_url}" "refs/tags/${name}"
echo "==> pushed"
