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
  echo "PWD=$(pwd)"
  echo "ARGS=$*"
} >> "$LOG"
exit 0
EOF
chmod +x "$TMP/go"

fails=0
run() { # run <logfile> <env-assignments...> -- runs the entrypoint in a fresh workspace
  local log="$1"; shift
  : >"$log"
  ( cd "$TMP/ws" && env "$@" LOG="$log" PATH="$TMP:$PATH" bash "$ENTRY" >/dev/null 2>&1 )
}
check() { # check <logfile> <grep-pattern> <message>
  grep -q "$2" "$1" || { echo "FAIL: $3"; echo "--- $1 ---"; cat "$1"; fails=$((fails+1)); }
}

mkdir -p "$TMP/ws/api"

# 1. Default toolchain is `auto` (the fix).
run "$TMP/1.log" PLUGIN_COMMAND="test ./..."
check "$TMP/1.log" '^GOTOOLCHAIN=auto$' "default GOTOOLCHAIN should be auto"

# 2. PLUGIN_TOOLCHAIN=local is respected (hermetic escape hatch).
run "$TMP/2.log" PLUGIN_COMMAND="build ./..." PLUGIN_TOOLCHAIN="local"
check "$TMP/2.log" '^GOTOOLCHAIN=local$' "PLUGIN_TOOLCHAIN=local should be honored"

# 3. PLUGIN_COMMAND required -> exit 2, go never invoked.
: >"$TMP/3.log"
( cd "$TMP/ws" && env LOG="$TMP/3.log" PATH="$TMP:$PATH" bash "$ENTRY" >/dev/null 2>&1 )
rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: missing PLUGIN_COMMAND should exit 2 (got $rc)"; fails=$((fails+1)); }
[ -s "$TMP/3.log" ] && { echo "FAIL: go was invoked despite missing PLUGIN_COMMAND"; fails=$((fails+1)); }

# 4. PLUGIN_WORKING_DIR cds before running go.
run "$TMP/4.log" PLUGIN_COMMAND="vet ./..." PLUGIN_WORKING_DIR="api"
check "$TMP/4.log" "PWD=$TMP/ws/api\$" "PLUGIN_WORKING_DIR should cd into the subdir"

# 5. cgo=false -> CGO_ENABLED=0.
run "$TMP/5.log" PLUGIN_COMMAND="build ./..." PLUGIN_CGO="false"
check "$TMP/5.log" '^CGO_ENABLED=0$' "PLUGIN_CGO=false should set CGO_ENABLED=0"

# 6. GOMODCACHE redirected into the workspace (default .go-mod).
run "$TMP/6.log" PLUGIN_COMMAND="test ./..."
check "$TMP/6.log" '^GOMODCACHE=.go-mod$' "GOMODCACHE should be redirected to .go-mod"

if [ "$fails" -eq 0 ]; then
  echo "PASS: go entrypoint"
else
  echo "FAILED: $fails check(s)"; exit 1
fi
