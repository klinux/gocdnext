#!/usr/bin/env bash
# Mock-PATH unit test for the playwright plugin entrypoint. A stub `npx` records
# the argv it was exec'd with so the dispatch is asserted without a real browser
# run. The test runs in a workspace with NO package.json, so detect_manager
# returns "none" and the dependency-install step is skipped — the entrypoint
# goes straight to `npx playwright ...`. Load-bearing cases:
#   - the default reporter combo (junit,html) and command (test) are assembled;
#   - modelled inputs (PLUGIN_PROJECT/GREP/...) become their CLI flags;
#   - PLUGIN_EXTRA_ARGS word-split does NOT glob-expand against the workspace
#     (#361): a `*` must reach playwright literally, not be rewritten into the
#     spec files sitting in the working dir.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"; mkdir -p "$TMP/home"
trap 'rm -rf "$TMP"' EXIT

# Stub npx: log each received arg on its own line, exit 0.
cat >"$TMP/npx" <<'EOF'
#!/usr/bin/env bash
: >"$ARGV_LOG"
for a in "$@"; do printf 'ARG=%s\n' "$a" >> "$ARGV_LOG"; done
exit 0
EOF
chmod +x "$TMP/npx"

fails=0
# $1 = workspace dir, rest = env. HOME isolated (the entrypoint doesn't touch
# git here, but keep it hermetic and consistent with the other harnesses).
run() {
  local ws="$1"; shift
  rm -f "$TMP/argv.log"
  ( cd "$ws" && env "$@" HOME="$TMP/home" ARGV_LOG="$TMP/argv.log" PATH="$TMP:$PATH" \
      bash "$ENTRY" >"$TMP/out" 2>&1 )
  echo $?
}
assert_argv() { # assert_argv <expected args...> -- exact argv vector in argv.log
  local got expected
  got="$(grep '^ARG=' "$TMP/argv.log")"
  expected="$(printf 'ARG=%s\n' "$@")"
  [ "$got" = "$expected" ]
}

# 1. defaults: command `test`, reporter combo junit,html, no extra args.
mkdir -p "$TMP/ws1"
rc=$(run "$TMP/ws1")
[ "$rc" = 0 ] || { echo "FAIL: default run should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
assert_argv playwright test --reporter=junit,html \
  || { echo "FAIL: default argv should be [playwright test --reporter=junit,html]"; cat "$TMP/argv.log"; fails=$((fails+1)); }

# 2. modelled inputs become CLI flags (quoted, so no splitting of their values).
mkdir -p "$TMP/ws2"
rc=$(run "$TMP/ws2" PLUGIN_PROJECT="chromium" PLUGIN_GREP="@smoke" PLUGIN_REPORTER="line")
[ "$rc" = 0 ] || { echo "FAIL: flags run should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
assert_argv playwright test --project=chromium --grep=@smoke --reporter=line \
  || { echo "FAIL: modelled inputs did not map to the expected flags"; cat "$TMP/argv.log"; fails=$((fails+1)); }

# 3. #361: PLUGIN_EXTRA_ARGS word-split does NOT glob-expand against the
#    workspace. Seed the dir with decoy spec files; a `*` token must reach
#    playwright literally. Assert the EXACT vector so an inflated argv (globbed)
#    is caught.
mkdir -p "$TMP/ws3"; : >"$TMP/ws3/a.spec.ts"; : >"$TMP/ws3/b.spec.ts"
rc=$(run "$TMP/ws3" PLUGIN_EXTRA_ARGS="*.spec.ts")
[ "$rc" = 0 ] || { echo "FAIL: glob case should exit 0 (got $rc)"; cat "$TMP/out"; fails=$((fails+1)); }
assert_argv playwright test --reporter=junit,html '*.spec.ts' \
  || { echo "FAIL: PLUGIN_EXTRA_ARGS glob-expanded against the workspace"; cat "$TMP/argv.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: playwright entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
