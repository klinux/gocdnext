#!/usr/bin/env bash
# Mock-PATH unit test for the dotnet plugin entrypoint. No real SDK: a stub
# `dotnet` records argv (and answers --list-sdks for the pin branch) so the
# dispatch is asserted offline. Load-bearing cases:
#   - PLUGIN_COMMAND is required (exit 2) and is word-split into dotnet's argv;
#   - PLUGIN_WORKING_DIR is honored (dotnet runs in that directory);
#   - an unknown PLUGIN_SDK fails loud (exit 2), not silently;
#   - the command word-split does NOT glob-expand against the working dir
#     (#361): a literal `*` must reach dotnet, never be rewritten into files.
#
# The stub records each arg on its OWN line (ARG=<arg>), never "$*" — a joined
# "$*" collapses argv boundaries, so `test -c Release` (3 args) and the single
# arg "test -c Release" would look identical and the word-split/glob cases
# would false-green. assert_argv compares the exact ARG= vector (order + count
# + boundaries).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"; mkdir -p "$TMP/home"
trap 'rm -rf "$TMP"' EXIT

# Stub dotnet: --list-sdks prints a fake inventory and exits WITHOUT logging
# (so a PWD= line marks a real user-command dispatch). Otherwise log PWD + one
# ARG= line per argument so argv boundaries are preserved exactly.
cat >"$TMP/dotnet" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = "--list-sdks" ]; then
  printf '8.0.118 [/usr/share/dotnet/sdk]\n10.0.100 [/usr/share/dotnet/sdk]\n'
  exit 0
fi
printf 'PWD=%s\n' "$(pwd)" >> "$LOG"
for a in "$@"; do printf 'ARG=%s\n' "$a" >> "$LOG"; done
exit 0
EOF
chmod +x "$TMP/dotnet"

fails=0
# Run the entrypoint with the stub ahead on PATH; HOME isolated so the
# `git config --global safe.directory` line can't touch the real gitconfig.
# Each run starts in a fresh CWD ($TMP) unless PLUGIN_WORKING_DIR is given.
run() {
  local log="$1"; shift
  : >"$log"
  ( cd "$TMP" && env "$@" HOME="$TMP/home" LOG="$log" PATH="$TMP:$PATH" \
      sh "$ENTRY" >"$log.out" 2>&1 )
  echo $?
}
# A user command was dispatched iff the stub logged a PWD line (the --list-sdks
# probe exits before logging).
dispatched() { grep -q '^PWD=' "$1"; }
# Exact argv vector: the ARG= lines must equal, in order, the expected args.
assert_argv() {
  local log="$1"; shift
  local got expected
  got="$(grep '^ARG=' "$log")"
  expected="$(printf 'ARG=%s\n' "$@")"
  [ "$got" = "$expected" ]
}

# 1. PLUGIN_COMMAND required -> exit 2, dotnet never dispatched.
rc=$(run "$TMP/1.log")
[ "$rc" = 2 ] || { echo "FAIL: missing PLUGIN_COMMAND should exit 2 (got $rc)"; fails=$((fails+1)); }
dispatched "$TMP/1.log" && { echo "FAIL: missing PLUGIN_COMMAND must not dispatch dotnet"; fails=$((fails+1)); }

# 2. command is word-split into EXACTLY the argv [test, -c, Release].
rc=$(run "$TMP/2.log" PLUGIN_COMMAND="test -c Release")
[ "$rc" = 0 ] || { echo "FAIL: valid command should exit 0 (got $rc)"; cat "$TMP/2.log.out"; fails=$((fails+1)); }
assert_argv "$TMP/2.log" test -c Release \
  || { echo "FAIL: command not word-split into exact argv [test -c Release]"; cat "$TMP/2.log"; fails=$((fails+1)); }

# 3. PLUGIN_WORKING_DIR is honored — dotnet runs in that directory.
mkdir -p "$TMP/wd"
rc=$(run "$TMP/3.log" PLUGIN_COMMAND="build" PLUGIN_WORKING_DIR="$TMP/wd")
[ "$rc" = 0 ] || { echo "FAIL: working-dir should exit 0 (got $rc)"; cat "$TMP/3.log.out"; fails=$((fails+1)); }
grep -qxF "PWD=$TMP/wd" "$TMP/3.log" \
  || { echo "FAIL: dotnet did not run in PLUGIN_WORKING_DIR"; cat "$TMP/3.log"; fails=$((fails+1)); }

# 4. an unknown SDK major fails loud (exit 2), before user-command dispatch.
#    (`dotnet --list-sdks` IS called intentionally — to print the inventory in
#    the error — so "no dispatch" means no PWD line, not "dotnet never ran".)
rc=$(run "$TMP/4.log" PLUGIN_COMMAND="build" PLUGIN_SDK="9")
[ "$rc" = 2 ] || { echo "FAIL: unknown sdk should exit 2 (got $rc)"; cat "$TMP/4.log.out"; fails=$((fails+1)); }
dispatched "$TMP/4.log" && { echo "FAIL: unknown sdk must not dispatch the user command"; fails=$((fails+1)); }

# 5. word-split does NOT glob-expand against the working dir (#361): the `*`
#    token must reach dotnet as a literal arg, not be rewritten into the two
#    .nupkg files sitting in the working dir (which would inflate the vector).
mkdir -p "$TMP/ws"; : >"$TMP/ws/pkg-a.nupkg"; : >"$TMP/ws/pkg-b.nupkg"
rc=$(run "$TMP/5.log" PLUGIN_WORKING_DIR="$TMP/ws" \
      PLUGIN_COMMAND="nuget push *.nupkg --source gocdnext")
[ "$rc" = 0 ] || { echo "FAIL: glob case should exit 0 (got $rc)"; cat "$TMP/5.log.out"; fails=$((fails+1)); }
assert_argv "$TMP/5.log" nuget push '*.nupkg' --source gocdnext \
  || { echo "FAIL: command glob-expanded against the working dir"; cat "$TMP/5.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: dotnet entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
