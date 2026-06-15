#!/usr/bin/env bash
# Mock-PATH unit test for the release-pr entrypoint. Stubs `git` and
# `gh`, runs the entrypoint in a temp workspace, and asserts the
# branch/commit/push/PR calls + the security guards. No real network
# or repo is touched.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
mkstub() { # $1=dir
  # git stub: log calls; canned answers. The origin URL carries an
  # embedded clone token so the test can prove it gets stripped.
  cat >"$1/git" <<EOF
#!/usr/bin/env bash
echo "git \$*" >> "$1/calls.log"
case "\$*" in
  "remote get-url origin") echo "https://x-access-token:CLONETOK@github.com/org/repo.git" ;;
  "diff --cached --quiet") exit 1 ;;
  "ls-remote --heads "*) [ -n "\${LSREMOTE_SHA:-}" ] && printf '%s\trefs/heads/x\n' "\${LSREMOTE_SHA}" ;;
esac
exit 0
EOF
  cat >"$1/gh" <<EOF
#!/usr/bin/env bash
echo "gh \$*" >> "$1/calls.log"
case "\$*" in
  *"--json url"*) echo "https://github.com/org/repo/pull/7" ;;
  "pr view "*)    exit 1 ;;
esac
exit 0
EOF
  chmod +x "$1/git" "$1/gh"
}

# run_ep <tmpdir> <env...>  → runs entrypoint in tmp/work, returns rc
run_ep() {
  local tmp="$1"; shift
  mkdir -p "$tmp/work"; mkstub "$tmp"
  ( cd "$tmp/work"; env PATH="$tmp:$PATH" GOCDNEXT_OUTPUT_FILE="$tmp/out.env" "$@" \
      bash "$HERE/entrypoint.sh" ) >"$tmp/stdout" 2>&1
}

fail() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- log ---"; cat "$2" 2>/dev/null; }; exit 1; }

# ── 1. happy path: bare version, branch from base, clean-URL push ──
T1="$(mktemp -d)"; trap 'rm -rf "$T1"' EXIT
mkdir -p "$T1/work"; echo "notes" > "$T1/work/NOTES.md"
run_ep "$T1" PLUGIN_VERSION=1.3.0 PLUGIN_TOKEN=tok PLUGIN_REPO=org/repo PLUGIN_NOTES_FILE=NOTES.md \
  || fail "happy path exited non-zero" "$T1/calls.log"
L="$T1/calls.log"
grep -q "git fetch https://github.com/org/repo.git main" "$L" || fail "base not fetched via clean URL" "$L"
grep -q "git checkout -B release/v1.3.0 FETCH_HEAD" "$L"      || fail "branch not based on FETCH_HEAD" "$L"
[ "$(cat "$T1/work/VERSION")" = "1.3.0" ]                     || fail "VERSION not bumped to bare 1.3.0" "$L"
grep -q "git commit -m release v1.3.0" "$L"                  || fail "no release commit" "$L"
grep -q "git push --force-with-lease=refs/heads/release/v1.3.0: https://github.com/org/repo.git HEAD:refs/heads/release/v1.3.0" "$L" \
  || fail "new branch: push not force-with-lease(empty) / clean URL / right ref" "$L"
grep -q "CLONETOK" "$L" && fail "embedded clone token leaked into a git call" "$L"
grep -q "tok@" "$L"     && fail "push token appeared on argv" "$L"
grep -q "gh pr create --repo org/repo --base main --head release/v1.3.0 --title Release v1.3.0 --body-file NOTES.md --label release" "$L" \
  || fail "pr create args wrong" "$L"
grep -q "pr_url=https://github.com/org/repo/pull/7" "$T1/out.env" || fail "pr_url output missing"
grep -q "release_tag=v1.3.0" "$T1/out.env"                       || fail "release_tag output missing"

# ── 2. prefixed version (what semver-bump prefix:v emits) normalises ──
T2="$(mktemp -d)"; trap 'rm -rf "$T1" "$T2"' EXIT
run_ep "$T2" PLUGIN_VERSION=v1.3.0 PLUGIN_TOKEN=tok PLUGIN_REPO=org/repo \
  || fail "prefixed version exited non-zero" "$T2/calls.log"
grep -q "git checkout -B release/v1.3.0 FETCH_HEAD" "$T2/calls.log" || fail "prefixed version → double-prefix branch" "$T2/calls.log"
[ "$(cat "$T2/work/VERSION")" = "1.3.0" ] || fail "prefixed version → VERSION not normalised to bare"

# ── 3. path traversal / absolute notes_file is rejected (token leak) ──
T3="$(mktemp -d)"; trap 'rm -rf "$T1" "$T2" "$T3"' EXIT
run_ep "$T3" PLUGIN_VERSION=1.3.0 PLUGIN_TOKEN=tok PLUGIN_NOTES_FILE=/proc/self/environ \
  && fail "absolute notes_file was accepted (token-leak vector)" "$T3/stdout"
grep -q "gh pr" "$T3/calls.log" 2>/dev/null && fail "gh ran despite bad notes_file" "$T3/calls.log"

# ── 4. version_file traversal rejected ──
T4="$(mktemp -d)"; trap 'rm -rf "$T1" "$T2" "$T3" "$T4"' EXIT
run_ep "$T4" PLUGIN_VERSION=1.3.0 PLUGIN_TOKEN=tok PLUGIN_VERSION_FILE=../escape \
  && fail "traversing version_file was accepted"

# ── 5. non-SemVer / injection version rejected ──
T5="$(mktemp -d)"; trap 'rm -rf "$T1" "$T2" "$T3" "$T4" "$T5"' EXIT
run_ep "$T5" PLUGIN_VERSION="1.3.0
pr_url=evil" PLUGIN_TOKEN=tok \
  && fail "newline/injection version was accepted (output-injection vector)"

# ── 6. existing remote branch → lease against its sha (rerun path) ──
T6="$(mktemp -d)"; trap 'rm -rf "$T1" "$T2" "$T3" "$T4" "$T5" "$T6"' EXIT
run_ep "$T6" LSREMOTE_SHA=deadbeefcafe PLUGIN_VERSION=1.3.0 PLUGIN_TOKEN=tok PLUGIN_REPO=org/repo \
  || fail "rerun (existing branch) exited non-zero" "$T6/calls.log"
grep -q "git push --force-with-lease=refs/heads/release/v1.3.0:deadbeefcafe https://github.com/org/repo.git HEAD:refs/heads/release/v1.3.0" "$T6/calls.log" \
  || fail "existing branch: lease not pinned to the remote sha" "$T6/calls.log"

echo "PASS: release-pr entrypoint (6 cases)"
