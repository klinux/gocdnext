#!/usr/bin/env bash
# Mock-PATH unit test for the dotnet plugin entrypoint. No real SDK: a stub
# `dotnet` records argv (and answers --list-sdks for the pin branch) so the
# dispatch is asserted offline. Load-bearing cases:
#   - PLUGIN_COMMAND is required (exit 2) and is word-split into dotnet's argv;
#   - PLUGIN_WORKING_DIR is honored (dotnet runs in that directory);
#   - an unknown PLUGIN_SDK fails loud (exit 2), not silently;
#   - the command word-split does NOT glob-expand against the working dir
#     (#361): a literal `*` must reach dotnet, never be rewritten into files.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"; mkdir -p "$TMP/home"
trap 'rm -rf "$TMP"' EXIT

# Stub dotnet: --list-sdks prints a fake inventory; anything else logs argv
# (one line, space-joined) and the real CWD so working-dir can be asserted.
cat >"$TMP/dotnet" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = "--list-sdks" ]; then
  printf '8.0.118 [/usr/share/dotnet/sdk]\n10.0.100 [/usr/share/dotnet/sdk]\n'
  exit 0
fi
printf 'ARGS=%s\n' "$*" >> "$LOG"
printf 'PWD=%s\n'  "$(pwd)" >> "$LOG"
exit 0
EOF
chmod +x "$TMP/dotnet"

fails=0
# Run the entrypoint with the stub ahead on PATH; HOME isolated so the
# `git config --global safe.directory` line can't touch the real gitconfig.
# Each run starts in a fresh, empty CWD unless PLUGIN_WORKING_DIR is given.
run() {
  local log="$1"; shift
  : >"$log"
  ( cd "$TMP" && env "$@" HOME="$TMP/home" LOG="$log" PATH="$TMP:$PATH" \
      sh "$ENTRY" >"$log.out" 2>&1 )
  echo $?
}
called() { grep -q '^ARGS=' "$1"; }

# 1. PLUGIN_COMMAND required -> exit 2, dotnet never dispatched.
rc=$(run "$TMP/1.log")
[ "$rc" = 2 ] || { echo "FAIL: missing PLUGIN_COMMAND should exit 2 (got $rc)"; fails=$((fails+1)); }
called "$TMP/1.log" && { echo "FAIL: missing PLUGIN_COMMAND must not dispatch dotnet"; fails=$((fails+1)); }

# 2. command is word-split verbatim into dotnet's argv.
rc=$(run "$TMP/2.log" PLUGIN_COMMAND="test -c Release")
[ "$rc" = 0 ] || { echo "FAIL: valid command should exit 0 (got $rc)"; cat "$TMP/2.log.out"; fails=$((fails+1)); }
grep -qxF "ARGS=test -c Release" "$TMP/2.log" \
  || { echo "FAIL: command not word-split verbatim"; cat "$TMP/2.log"; fails=$((fails+1)); }

# 3. PLUGIN_WORKING_DIR is honored — dotnet runs in that directory.
mkdir -p "$TMP/wd"
rc=$(run "$TMP/3.log" PLUGIN_COMMAND="build" PLUGIN_WORKING_DIR="$TMP/wd")
[ "$rc" = 0 ] || { echo "FAIL: working-dir should exit 0 (got $rc)"; cat "$TMP/3.log.out"; fails=$((fails+1)); }
grep -qxF "PWD=$TMP/wd" "$TMP/3.log" \
  || { echo "FAIL: dotnet did not run in PLUGIN_WORKING_DIR"; cat "$TMP/3.log"; fails=$((fails+1)); }

# 4. an unknown SDK major fails loud (exit 2), before any dispatch.
rc=$(run "$TMP/4.log" PLUGIN_COMMAND="build" PLUGIN_SDK="9")
[ "$rc" = 2 ] || { echo "FAIL: unknown sdk should exit 2 (got $rc)"; cat "$TMP/4.log.out"; fails=$((fails+1)); }
called "$TMP/4.log" && { echo "FAIL: unknown sdk must not dispatch dotnet"; fails=$((fails+1)); }

# 5. word-split does NOT glob-expand against the working dir (#361): a `*`
#    token must reach dotnet literally, not be rewritten into workspace files.
mkdir -p "$TMP/ws"; : >"$TMP/ws/pkg-a.nupkg"; : >"$TMP/ws/pkg-b.nupkg"
rc=$(run "$TMP/5.log" PLUGIN_WORKING_DIR="$TMP/ws" \
      PLUGIN_COMMAND="nuget push *.nupkg --source gocdnext")
[ "$rc" = 0 ] || { echo "FAIL: glob case should exit 0 (got $rc)"; cat "$TMP/5.log.out"; fails=$((fails+1)); }
grep -qxF "ARGS=nuget push *.nupkg --source gocdnext" "$TMP/5.log" \
  || { echo "FAIL: command glob-expanded against the working dir"; cat "$TMP/5.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: dotnet entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
