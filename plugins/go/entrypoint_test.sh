#!/usr/bin/env bash
# Mock-PATH test for the go plugin entrypoint. The load-bearing case
# (issue surfaced by backoffice-shell PR #35): the official golang image
# pins GOTOOLCHAIN=local, so a project whose go.mod requires a newer Go
# than the image fails hard. The plugin must default GOTOOLCHAIN=auto so
# the toolchain is honored (downloaded into $GOMODCACHE, which the cache:
# block tars), while still allowing PLUGIN_TOOLCHAIN=local for hermetic
# builds. Also guards the existing contract: PLUGIN_COMMAND required,
# working-dir cd, cgo parse, GOMODCACHE/GOCACHE redirect.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Mock `go`: record the env the entrypoint exported + argv, then exit 0.
cat >"$TMP/go" <<'EOF'
#!/usr/bin/env bash
{
  echo "GOTOOLCHAIN=${GOTOOLCHAIN:-<unset>}"
  echo "CGO_ENABLED=${CGO_ENABLED:-<unset>}"
  echo "GOMODCACHE=${GOMODCACHE:-<unset>}"
  echo "GOCACHE=${GOCACHE:-<unset>}"
  echo "PWD=$(pwd)"
  echo "ARGS=$*"
} >> "$LOG"
exit 0
EOF
chmod +x "$TMP/go"

fails=0
# HOME is isolated to $TMP/home on every entrypoint run: the entrypoint calls
# `git config --global --add safe.directory '*'`, which would otherwise mutate
# the runner's real ~/.gitconfig (permanently disabling Git's dubious-ownership
# protection for that user). Scoping HOME keeps the test hermetic.
run() { # run <logfile> <env-assignments...> -- runs the entrypoint in a fresh workspace
  local log="$1"; shift
  : >"$log"
  ( cd "$TMP/ws" && env "$@" HOME="$TMP/home" LOG="$log" PATH="$TMP:$PATH" bash "$ENTRY" >/dev/null 2>&1 )
}
check() { # check <logfile> <grep-pattern> <message>
  grep -q "$2" "$1" || { echo "FAIL: $3"; echo "--- $1 ---"; cat "$1"; fails=$((fails+1)); }
}

mkdir -p "$TMP/ws/api" "$TMP/home"

# 1. Default toolchain is `auto` (the fix).
run "$TMP/1.log" PLUGIN_COMMAND="test ./..."
check "$TMP/1.log" '^GOTOOLCHAIN=auto$' "default GOTOOLCHAIN should be auto"

# 2. PLUGIN_TOOLCHAIN=local is respected (hermetic escape hatch).
run "$TMP/2.log" PLUGIN_COMMAND="build ./..." PLUGIN_TOOLCHAIN="local"
check "$TMP/2.log" '^GOTOOLCHAIN=local$' "PLUGIN_TOOLCHAIN=local should be honored"

# 3. PLUGIN_COMMAND required -> exit 2, go never invoked.
: >"$TMP/3.log"
( cd "$TMP/ws" && env HOME="$TMP/home" LOG="$TMP/3.log" PATH="$TMP:$PATH" bash "$ENTRY" >/dev/null 2>&1 )
rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: missing PLUGIN_COMMAND should exit 2 (got $rc)"; fails=$((fails+1)); }
[ -s "$TMP/3.log" ] && { echo "FAIL: go was invoked despite missing PLUGIN_COMMAND"; fails=$((fails+1)); }

# 4. PLUGIN_WORKING_DIR cds before running go.
run "$TMP/4.log" PLUGIN_COMMAND="vet ./..." PLUGIN_WORKING_DIR="api"
check "$TMP/4.log" "PWD=$TMP/ws/api\$" "PLUGIN_WORKING_DIR should cd into the subdir"

# 5. cgo=false -> CGO_ENABLED=0.
run "$TMP/5.log" PLUGIN_COMMAND="build ./..." PLUGIN_CGO="false"
check "$TMP/5.log" '^CGO_ENABLED=0$' "PLUGIN_CGO=false should set CGO_ENABLED=0"

# 6. GOMODCACHE + GOCACHE default to ABSOLUTE paths under the workspace (Go 1.26
# rejects relative for BOTH).
run "$TMP/6.log" PLUGIN_COMMAND="test ./..."
check "$TMP/6.log" "^GOMODCACHE=$TMP/ws/.go-mod\$" "GOMODCACHE should be an absolute .go-mod under the workspace"
check "$TMP/6.log" "^GOCACHE=$TMP/ws/.go-cache\$" "GOCACHE should be an absolute .go-cache under the workspace"

# 7. An explicit RELATIVE override (variables:/profile/env) is normalised to
# absolute — the corner case a plain `${VAR:-default}` would miss.
run "$TMP/7.log" PLUGIN_COMMAND="test ./..." GOMODCACHE=".rel-mod" GOCACHE=".rel-cache"
check "$TMP/7.log" "^GOMODCACHE=$TMP/ws/.rel-mod\$" "relative GOMODCACHE override should be anchored to CWD"
check "$TMP/7.log" "^GOCACHE=$TMP/ws/.rel-cache\$" "relative GOCACHE override should be anchored to CWD"

# 8. An ABSOLUTE override is preserved as-is (use a creatable path — the
# entrypoint mkdir -p's it).
run "$TMP/8.log" PLUGIN_COMMAND="test ./..." GOMODCACHE="$TMP/abs-mod"
check "$TMP/8.log" "^GOMODCACHE=$TMP/abs-mod\$" "absolute GOMODCACHE override should be kept"

if [ "$fails" -eq 0 ]; then
  echo "PASS: go entrypoint"
else
  echo "FAILED: $fails check(s)"; exit 1
fi
