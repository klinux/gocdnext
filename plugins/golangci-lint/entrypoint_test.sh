#!/usr/bin/env bash
# Mock-PATH test for the golangci-lint plugin entrypoint. The load-bearing
# case: the three caches the linter touches — GOLANGCI_LINT_CACHE, GOMODCACHE
# and GOCACHE — must be ABSOLUTE. golangci-lint rejects a relative
# GOLANGCI_LINT_CACHE, and the `go list` it runs internally makes Go 1.26
# reject a relative GOMODCACHE / GOCACHE ("must be absolute path"). The
# entrypoint must anchor all three under the (working-)dir, for the default AND
# an explicit relative override, while preserving an absolute override. Also
# guards the working-dir cd and the args passthrough.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Mock `golangci-lint`: record the env the entrypoint exported + argv, exit 0.
cat >"$TMP/golangci-lint" <<'EOF'
#!/usr/bin/env bash
{
  echo "GOLANGCI_LINT_CACHE=${GOLANGCI_LINT_CACHE:-<unset>}"
  echo "GOMODCACHE=${GOMODCACHE:-<unset>}"
  echo "GOCACHE=${GOCACHE:-<unset>}"
  echo "PWD=$(pwd)"
  echo "ARGS=$*"
  # Also record argv one-per-line so a test can assert the EXACT vector
  # (order + count + boundaries); ARGS=$* joins the args and can't prove a
  # literal `*` was not glob-expanded into separate filename args.
  for a in "$@"; do echo "ARG=$a"; done
} >> "$LOG"
exit 0
EOF
chmod +x "$TMP/golangci-lint"

fails=0
# HOME is isolated to $TMP/home on every run: the entrypoint calls
# `git config --global --add safe.directory '*'`, which would otherwise mutate
# the runner's real ~/.gitconfig. Scoping HOME keeps the test hermetic.
run() { # run <logfile> <env-assignments...>
  local log="$1"; shift
  : >"$log"
  ( cd "$TMP/ws" && env "$@" HOME="$TMP/home" LOG="$log" PATH="$TMP:$PATH" sh "$ENTRY" >/dev/null 2>&1 )
}
check() { # check <logfile> <grep-pattern> <message>
  grep -q "$2" "$1" || { echo "FAIL: $3"; echo "--- $1 ---"; cat "$1"; fails=$((fails+1)); }
}
assert_argv() { # assert_argv <logfile> <expected args...> -- exact argv vector
  local log="$1"; shift
  local got expected
  got="$(grep '^ARG=' "$log")"
  expected="$(printf 'ARG=%s\n' "$@")"
  [ "$got" = "$expected" ]
}

mkdir -p "$TMP/ws/api" "$TMP/home"

# 1. All three caches default to ABSOLUTE paths under the workspace (the fix).
run "$TMP/1.log"
check "$TMP/1.log" "^GOLANGCI_LINT_CACHE=$TMP/ws/.golangci-cache\$" "GOLANGCI_LINT_CACHE should default to an absolute path under the workspace"
check "$TMP/1.log" "^GOMODCACHE=$TMP/ws/.go-mod\$" "GOMODCACHE should default to an absolute path under the workspace"
check "$TMP/1.log" "^GOCACHE=$TMP/ws/.go-cache\$" "GOCACHE should default to an absolute path under the workspace"

# 2. PLUGIN_WORKING_DIR cds first, so the caches anchor under the subdir.
run "$TMP/2.log" PLUGIN_WORKING_DIR="api"
check "$TMP/2.log" "^PWD=$TMP/ws/api\$" "PLUGIN_WORKING_DIR should cd into the subdir"
check "$TMP/2.log" "^GOLANGCI_LINT_CACHE=$TMP/ws/api/.golangci-cache\$" "cache should anchor under the working dir"

# 3. Explicit RELATIVE overrides (variables:/profile/env) are normalised to
# absolute — the corner case a plain `${VAR:-default}` would miss.
run "$TMP/3.log" GOLANGCI_LINT_CACHE=".rel-lint" GOMODCACHE=".rel-mod" GOCACHE=".rel-cache"
check "$TMP/3.log" "^GOLANGCI_LINT_CACHE=$TMP/ws/.rel-lint\$" "relative GOLANGCI_LINT_CACHE override should be anchored to CWD"
check "$TMP/3.log" "^GOMODCACHE=$TMP/ws/.rel-mod\$" "relative GOMODCACHE override should be anchored to CWD"
check "$TMP/3.log" "^GOCACHE=$TMP/ws/.rel-cache\$" "relative GOCACHE override should be anchored to CWD"

# 4. An ABSOLUTE override is preserved as-is (creatable path — entrypoint mkdir -p's it).
run "$TMP/4.log" GOLANGCI_LINT_CACHE="$TMP/abs-lint"
check "$TMP/4.log" "^GOLANGCI_LINT_CACHE=$TMP/abs-lint\$" "absolute GOLANGCI_LINT_CACHE override should be kept"

# 5. Args passthrough: PLUGIN_TIMEOUT + PLUGIN_ARGS land on the golangci-lint argv.
run "$TMP/5.log" PLUGIN_TIMEOUT="3m" PLUGIN_ARGS="./cmd/..."
check "$TMP/5.log" "^ARGS=run --timeout 3m ./cmd/...\$" "PLUGIN_TIMEOUT + PLUGIN_ARGS should pass through to golangci-lint run"

# 6. word-split does NOT glob-expand against the workspace (#361): a `*` token
# in PLUGIN_ARGS must reach golangci-lint literally, not be rewritten into the
# .go files sitting in the working dir (which would inflate the argv vector).
# Default timeout is 5m. Assert the EXACT vector [run, --timeout, 5m, *.go].
: >"$TMP/ws/main_a.go"; : >"$TMP/ws/main_b.go"
run "$TMP/6.log" PLUGIN_ARGS="*.go"
assert_argv "$TMP/6.log" run --timeout 5m '*.go' \
  || { echo "FAIL: PLUGIN_ARGS glob-expanded against the workspace"; echo "--- $TMP/6.log ---"; cat "$TMP/6.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then
  echo "PASS: golangci-lint entrypoint"
else
  echo "FAILED: $fails check(s)"; exit 1
fi
