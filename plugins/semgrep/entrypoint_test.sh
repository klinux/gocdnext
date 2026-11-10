#!/usr/bin/env bash
# Mock-PATH unit test for the semgrep plugin entrypoint. No real semgrep: a stub
# records the argv it was called with and writes a minimal (empty) SARIF to the
# --output path so the entrypoint's real jq severity gate runs and exits 0.
# Load-bearing cases:
#   - fail-on is validated (bad value -> exit 2);
#   - comma/space-separated CONFIG becomes one --config per entry;
#   - comma/space-separated PLUGIN_PATHS becomes one positional path per entry,
#     defaulting to "." when unset;
#   - NEITHER of the two unquoted loop-splits (CONFIG, PLUGIN_PATHS) glob-expands
#     against the workspace (#361): a literal `*` must reach semgrep, never be
#     rewritten into the files sitting in the working dir.
#
# Requires jq (present on CI's ubuntu runner and in the plugin image).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"; mkdir -p "$TMP/home"
trap 'rm -rf "$TMP"' EXIT

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not available"; exit 0; }

# Stub semgrep: log each arg on its own line, write an empty-but-valid SARIF to
# the file named after --output so the entrypoint's jq gate has something to read.
cat >"$TMP/semgrep" <<'EOF'
#!/usr/bin/env bash
out=""; prev=""
: >"$ARGV_LOG"
for a in "$@"; do
  printf 'ARG=%s\n' "$a" >> "$ARGV_LOG"
  [ "$prev" = "--output" ] && out="$a"
  prev="$a"
done
[ -n "$out" ] && printf '%s' '{"runs":[{"tool":{"driver":{"rules":[]}},"results":[]}]}' > "$out"
exit 0
EOF
chmod +x "$TMP/semgrep"

fails=0
# Run the entrypoint in a fresh workspace with the stub ahead on PATH. HOME is
# isolated so the `git config --global safe.directory` line can't touch the
# runner's real gitconfig. $1 = workspace dir, rest = env assignments.
run() {
  local ws="$1"; shift
  ( cd "$ws" && env "$@" HOME="$TMP/home" ARGV_LOG="$TMP/argv.log" PATH="$TMP:$PATH" \
      sh "$ENTRY" >"$TMP/out" 2>&1 )
  echo $?
}

# 1. invalid fail-on -> exit 2, semgrep never run.
mkdir -p "$TMP/ws1"; rm -f "$TMP/argv.log"
rc=$(run "$TMP/ws1" PLUGIN_FAIL_ON="bogus")
[ "$rc" = 2 ] || { echo "FAIL: invalid fail-on should exit 2 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
[ -f "$TMP/argv.log" ] && { echo "FAIL: semgrep must not run on invalid fail-on"; fails=$((fails+1)); }

# 2. CONFIG split: "p/ci, p/secrets" -> two --config flags; PLUGIN_PATHS unset
#    defaults the scan target to ".".
mkdir -p "$TMP/ws2"
rc=$(run "$TMP/ws2" PLUGIN_CONFIG="p/ci, p/secrets")
[ "$rc" = 0 ] || { echo "FAIL: valid scan should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
n_cfg=$(grep -cxF 'ARG=--config' "$TMP/argv.log")
[ "$n_cfg" = 2 ] || { echo "FAIL: expected two --config flags, got $n_cfg"; cat "$TMP/argv.log"; fails=$((fails+1)); }
grep -qxF 'ARG=p/ci'      "$TMP/argv.log" || { echo "FAIL: CONFIG 'p/ci' not passed as a --config value"; cat "$TMP/argv.log"; fails=$((fails+1)); }
grep -qxF 'ARG=p/secrets' "$TMP/argv.log" || { echo "FAIL: CONFIG 'p/secrets' not passed as a --config value"; cat "$TMP/argv.log"; fails=$((fails+1)); }
tail -1 "$TMP/argv.log" | grep -qxF 'ARG=.' || { echo "FAIL: default scan target should be '.'"; cat "$TMP/argv.log"; fails=$((fails+1)); }

# 3. PLUGIN_PATHS split: "src, test" -> two positional paths.
mkdir -p "$TMP/ws3"
rc=$(run "$TMP/ws3" PLUGIN_CONFIG="p/default" PLUGIN_PATHS="src, test")
[ "$rc" = 0 ] || { echo "FAIL: paths scan should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
grep -qxF 'ARG=src'  "$TMP/argv.log" || { echo "FAIL: path 'src' not passed"; cat "$TMP/argv.log"; fails=$((fails+1)); }
grep -qxF 'ARG=test' "$TMP/argv.log" || { echo "FAIL: path 'test' not passed"; cat "$TMP/argv.log"; fails=$((fails+1)); }

# 4. #361 (CONFIG): a `*` in PLUGIN_CONFIG reaches semgrep literally, not
#    rewritten into the decoy files sitting in the working dir.
mkdir -p "$TMP/ws4"; : >"$TMP/ws4/zz-decoy-a"; : >"$TMP/ws4/zz-decoy-b"
rc=$(run "$TMP/ws4" PLUGIN_CONFIG="*")
[ "$rc" = 0 ] || { echo "FAIL: glob-config scan should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
grep -qxF 'ARG=*' "$TMP/argv.log" \
  || { echo "FAIL: PLUGIN_CONFIG glob-expanded against the workspace"; cat "$TMP/argv.log"; fails=$((fails+1)); }
grep -qxF 'ARG=zz-decoy-a' "$TMP/argv.log" \
  && { echo "FAIL: PLUGIN_CONFIG glob-expanded (decoy filename leaked)"; cat "$TMP/argv.log"; fails=$((fails+1)); }

# 5. #361 (PLUGIN_PATHS): a `*` in PLUGIN_PATHS reaches semgrep literally.
mkdir -p "$TMP/ws5"; : >"$TMP/ws5/zz-decoy-a"; : >"$TMP/ws5/zz-decoy-b"
rc=$(run "$TMP/ws5" PLUGIN_CONFIG="p/default" PLUGIN_PATHS="*")
[ "$rc" = 0 ] || { echo "FAIL: glob-paths scan should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
grep -qxF 'ARG=*' "$TMP/argv.log" \
  || { echo "FAIL: PLUGIN_PATHS glob-expanded against the workspace"; cat "$TMP/argv.log"; fails=$((fails+1)); }
grep -qxF 'ARG=zz-decoy-a' "$TMP/argv.log" \
  && { echo "FAIL: PLUGIN_PATHS glob-expanded (decoy filename leaked)"; cat "$TMP/argv.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: semgrep entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
