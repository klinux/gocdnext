#!/bin/sh
# gocdnext/gh-pages entrypoint — see Dockerfile for the contract.

set -eu

fail() { echo "gocdnext/gh-pages: $1" >&2; exit 2; }

if [ -n "${PLUGIN_WORKING_DIR:-}" ]; then
    cd "${PLUGIN_WORKING_DIR}"
fi

DIR="${PLUGIN_DIR:-}"
[ -n "${DIR}" ] || fail "dir: is required (the built site directory, e.g. dist/)"
[ -d "${DIR}" ] || fail "dir '${DIR}' not found in the workspace"
[ -n "$(ls -A "${DIR}" 2>/dev/null)" ] || fail "dir '${DIR}' is empty — did the build job run?"

BRANCH="${PLUGIN_BRANCH:-gh-pages}"

git config --global --add safe.directory '*' 2>/dev/null || true

# Remote resolution: explicit input wins; otherwise derive from
# the workspace clone's origin, STRIPPING any embedded credentials
# (the agent's clone token may ride the URL and is read-only /
# short-lived — pushes need the operator-provided token).
REMOTE="${PLUGIN_REMOTE_URL:-}"
if [ -z "${REMOTE}" ]; then
    ORIGIN=$(git remote get-url origin 2>/dev/null) || fail "no remote-url: input and the workspace has no git origin"
    case "${ORIGIN}" in
        https://*)
            HOSTPATH="${ORIGIN#https://}"
            HOSTPATH="${HOSTPATH#*@}"   # drop user:token@ if present
            REMOTE="https://${HOSTPATH}"
            ;;
        *) fail "origin '${ORIGIN%%:*}:…' is not https — pass remote-url: explicitly" ;;
    esac
fi
[ -n "${GIT_TOKEN:-}" ] || fail "GIT_TOKEN env is required (secrets: [GIT_TOKEN] — a token with push access)"

if [ -n "${PLUGIN_CNAME:-}" ]; then
    printf '%s\n' "${PLUGIN_CNAME}" > "${DIR}/CNAME"
fi
# Pages serves _-prefixed dirs (Astro/Next assets) only with this.
touch "${DIR}/.nojekyll"

MSG="${PLUGIN_COMMIT_MESSAGE:-deploy: ${CI_COMMIT_SHORT_SHA:-$(date -u +%Y%m%dT%H%M%SZ)}}"

echo "==> publishing ${DIR} to ${BRANCH} (${REMOTE})"
cd "${DIR}"
rm -rf .git
git init -q -b "${BRANCH}"
git config user.name "gocdnext"
git config user.email "ci@gocdnext.invalid"
git add -A
git commit -q -m "${MSG}"
# Auth via GIT_ASKPASS: the push URL stays CLEAN — the token never
# appears on argv (visible in /proc//cmdline and `ps` for the
# process lifetime) nor in git's error output (which embeds the
# URL). Git invokes the askpass helper for username and password;
# the helper reads GIT_TOKEN from its inherited env.
ASKPASS="$(mktemp)"
trap 'rm -f "${ASKPASS}"' EXIT
cat > "${ASKPASS}" <<'EOF'
#!/bin/sh
case "$1" in
    Username*) echo "x-access-token" ;;
    *)         echo "${GIT_TOKEN}" ;;
esac
EOF
chmod 700 "${ASKPASS}"
export GIT_ASKPASS="${ASKPASS}"
export GIT_TERMINAL_PROMPT=0

# Force push: each deploy is a fresh orphan commit (see Dockerfile
# for why pages-branch history is deliberately not kept). No pipe
# on the push — a `| sed` would mask git's exit code and a FAILED
# push would print "published" (caught by the E2E smoke).
git push --force "${REMOTE}" "${BRANCH}:${BRANCH}" || fail "git push to ${BRANCH} failed — see output above"
echo "    published $(git rev-parse --short HEAD) to ${BRANCH}"
