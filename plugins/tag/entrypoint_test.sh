#!/usr/bin/env bash
# Mock-PATH test for the tag plugin entrypoint. The load-bearing case:
# the agent clones private repos with a token embedded in the origin
# URL, so `git remote get-url origin` returns
# https://x-access-token:CLONE@host/... — the push must NOT prepend a
# second user:token@ (which git reads as host:port → "Port number was
# not a decimal"). Auth rides GIT_ASKPASS, so no token hits argv.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/git" <<EOF
#!/usr/bin/env bash
echo "git \$*" >> "$TMP/calls.log"
case "\$*" in
  "remote get-url origin") echo "https://x-access-token:CLONETOK@github.com/org/repo.git" ;;
esac
exit 0
EOF
chmod +x "$TMP/git"

PLUGIN_NAME="v1.2.3" PLUGIN_TOKEN="RELEASETOK" PLUGIN_MESSAGE="Release v1.2.3" \
  PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1

L="$TMP/calls.log"
fail() { echo "FAIL: $1"; echo "--- calls ---"; cat "$L" 2>/dev/null; exit 1; }

grep -q "git tag -a v1.2.3" "$L" || fail "annotated tag not created"
# Push to the CLEAN origin URL (embedded creds stripped, no second
# user:token@) at the precise ref.
grep -q "git push https://github.com/org/repo.git refs/tags/v1.2.3" "$L" \
  || fail "push not to clean URL / right ref"
# Neither the release token nor the embedded clone token may reach argv.
grep -q "RELEASETOK" "$L" && fail "release token leaked onto git argv"
grep -q "CLONETOK" "$L" && fail "embedded clone token leaked onto git argv"

echo "PASS: tag entrypoint"
