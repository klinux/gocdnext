#!/usr/bin/env bash
# Mock-PATH unit test for the gitleaks plugin entrypoint. A stub `gitleaks`
# records argv so the dispatch is asserted without a real scan. Load-bearing
# cases:
#   - an unknown scan_mode fails loud (exit 2);
#   - a valid allowlist-paths input materialises a runtime TOML (useDefault +
#     one substring entry per path) and is passed via --config;
#   - a path-traversal allowlist entry is rejected (exit 2);
#   - the unquoted allowlist token split does NOT glob-expand against the
#     workspace (#361): `allowlist-paths='*'` must be rejected by the charset
#     guard (exit 2), NOT silently rewritten into the workspace's real
#     (charset-valid) filenames and accepted.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"; mkdir -p "$TMP/home"
RUNTIME_TOML=/tmp/gocdnext-gitleaks-runtime.toml
trap 'rm -rf "$TMP"; rm -f "$RUNTIME_TOML"' EXIT

# Stub gitleaks: log each arg on its own line, exit 0. Reached only when the
# entrypoint gets as far as `exec gitleaks` (i.e. no validation error first).
cat >"$TMP/gitleaks" <<'EOF'
#!/usr/bin/env bash
: >"$ARGV_LOG"
for a in "$@"; do printf 'ARG=%s\n' "$a" >> "$ARGV_LOG"; done
exit 0
EOF
chmod +x "$TMP/gitleaks"

fails=0
# $1 = workspace dir, rest = env. HOME isolated so `git config --global
# safe.directory` can't touch the runner's gitconfig.
run() {
  local ws="$1"; shift
  rm -f "$TMP/argv.log" "$RUNTIME_TOML"
  ( cd "$ws" && env "$@" HOME="$TMP/home" ARGV_LOG="$TMP/argv.log" PATH="$TMP:$PATH" \
      bash "$ENTRY" >"$TMP/out" 2>&1 )
  echo $?
}
called() { [ -f "$TMP/argv.log" ]; }

# 1. unknown scan_mode -> exit 2, gitleaks never run.
mkdir -p "$TMP/ws1"
rc=$(run "$TMP/ws1" PLUGIN_SCAN_MODE="bogus")
[ "$rc" = 2 ] || { echo "FAIL: unknown scan_mode should exit 2 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
called && { echo "FAIL: unknown scan_mode must not run gitleaks"; fails=$((fails+1)); }

# 2. valid allowlist-paths -> runtime TOML (useDefault + one entry per path),
#    passed via --config.
mkdir -p "$TMP/ws2"
rc=$(run "$TMP/ws2" PLUGIN_ALLOWLIST_PATHS="docs/, tests/")
[ "$rc" = 0 ] || { echo "FAIL: valid allowlist should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
called || { echo "FAIL: valid allowlist should reach gitleaks"; fails=$((fails+1)); }
grep -qxF 'ARG=--config' "$TMP/argv.log" || { echo "FAIL: runtime config not passed via --config"; cat "$TMP/argv.log"; fails=$((fails+1)); }
grep -qxF "ARG=$RUNTIME_TOML" "$TMP/argv.log" || { echo "FAIL: --config value is not the runtime TOML"; cat "$TMP/argv.log"; fails=$((fails+1)); }
[ -f "$RUNTIME_TOML" ] || { echo "FAIL: runtime TOML was not generated"; fails=$((fails+1)); }
grep -q 'useDefault = true' "$RUNTIME_TOML" || { echo "FAIL: runtime TOML missing useDefault (would disable all rules)"; cat "$RUNTIME_TOML"; fails=$((fails+1)); }
grep -q "docs/"  "$RUNTIME_TOML" || { echo "FAIL: 'docs/' not in runtime allowlist"; cat "$RUNTIME_TOML"; fails=$((fails+1)); }
grep -q "tests/" "$RUNTIME_TOML" || { echo "FAIL: 'tests/' not in runtime allowlist"; cat "$RUNTIME_TOML"; fails=$((fails+1)); }

# 3. path-traversal allowlist entry -> exit 2, gitleaks never run.
mkdir -p "$TMP/ws3"
rc=$(run "$TMP/ws3" PLUGIN_ALLOWLIST_PATHS="../etc")
[ "$rc" = 2 ] || { echo "FAIL: traversal allowlist should exit 2 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
called && { echo "FAIL: traversal allowlist must not run gitleaks"; fails=$((fails+1)); }

# 4. #361: the allowlist token split does NOT glob-expand against the workspace.
#    The workspace is seeded with charset-VALID decoy names (`alpha`, `beta`):
#    if `*` expanded, those names would pass the charset guard and be accepted
#    silently. With globbing off, `*` stays literal and the charset guard
#    rejects it (exit 2) — the intended loud failure. gitleaks never runs.
mkdir -p "$TMP/ws4"; : >"$TMP/ws4/alpha"; : >"$TMP/ws4/beta"
rc=$(run "$TMP/ws4" PLUGIN_ALLOWLIST_PATHS="*")
[ "$rc" = 2 ] || { echo "FAIL: allowlist-paths='*' should be rejected (exit 2), got $rc — glob-expanded against the workspace?"; cat "$TMP/out"; fails=$((fails+1)); }
called && { echo "FAIL: allowlist-paths='*' reached gitleaks — the '*' glob-expanded into valid filenames instead of being rejected"; cat "$TMP/argv.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: gitleaks entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
