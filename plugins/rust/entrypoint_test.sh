#!/usr/bin/env bash
# Mock-PATH test for the rust entrypoint — argv/env wiring without compiling
# anything (the real cargo build/clippy/fmt contract is in smoke_test.sh). Mock
# `cargo` + `rustup` record their argv and the exported env.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENTRY="$HERE/entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/cargo" <<'EOF'
#!/usr/bin/env bash
{ echo "ARGS=$*"; echo "CARGO_HOME=${CARGO_HOME:-<unset>}"; echo "RUSTFLAGS=${RUSTFLAGS:-<unset>}"; echo "PWD=$(pwd)"; } >> "$LOG"
exit 0
EOF
cat >"$TMP/rustup" <<'EOF'
#!/usr/bin/env bash
echo "RUSTUP=$*" >> "$LOG"
exit 0
EOF
chmod +x "$TMP/cargo" "$TMP/rustup"

fails=0
run() { local log="$1"; shift; : >"$log"; ( cd "${WD:-$TMP}" && env "$@" LOG="$log" PATH="$TMP:$PATH" bash "$ENTRY" >"$log.out" 2>&1 ); echo $?; }
called() { [ -s "$1" ]; }

# 1. PLUGIN_COMMAND required → exit 2, cargo never called.
rc=$(run "$TMP/1.log")
[ "$rc" = 2 ] || { echo "FAIL: missing PLUGIN_COMMAND should exit 2 (got $rc)"; fails=$((fails+1)); }
called "$TMP/1.log" && { echo "FAIL: missing PLUGIN_COMMAND must not call cargo"; fails=$((fails+1)); }

# 2. word-split argv + default relative CARGO_HOME under the workspace.
rc=$(run "$TMP/2.log" PLUGIN_COMMAND="test --workspace")
grep -qx "ARGS=test --workspace" "$TMP/2.log" || { echo "FAIL: argv word-split"; cat "$TMP/2.log"; fails=$((fails+1)); }
grep -qx "CARGO_HOME=.cargo-home" "$TMP/2.log" || { echo "FAIL: CARGO_HOME default"; cat "$TMP/2.log"; fails=$((fails+1)); }

# 3. RUSTFLAGS passthrough.
rc=$(run "$TMP/3.log" PLUGIN_COMMAND="build --release" PLUGIN_RUSTFLAGS="-D warnings")
grep -qx "RUSTFLAGS=-D warnings" "$TMP/3.log" || { echo "FAIL: RUSTFLAGS not exported"; cat "$TMP/3.log"; fails=$((fails+1)); }

# 4. a pinned toolchain is installed + defaulted before running (rustup path).
rc=$(run "$TMP/4.log" PLUGIN_COMMAND="build" PLUGIN_TOOLCHAIN="1.83.0")
grep -q "RUSTUP=toolchain install 1.83.0" "$TMP/4.log" || { echo "FAIL: toolchain not installed"; cat "$TMP/4.log"; fails=$((fails+1)); }
grep -q "RUSTUP=default 1.83.0" "$TMP/4.log" || { echo "FAIL: toolchain not defaulted"; cat "$TMP/4.log"; fails=$((fails+1)); }

# 5. stable (default) toolchain does NOT shell out to rustup.
rc=$(run "$TMP/5.log" PLUGIN_COMMAND="build")
grep -q "^RUSTUP=" "$TMP/5.log" && { echo "FAIL: stable should not call rustup"; cat "$TMP/5.log"; fails=$((fails+1)); }

# 6. PLUGIN_WORKING_DIR is honored (cargo runs in that dir).
mkdir -p "$TMP/proj"
rc=$(run "$TMP/6.log" PLUGIN_COMMAND="build" PLUGIN_WORKING_DIR="$TMP/proj")
grep -qx "PWD=$TMP/proj" "$TMP/6.log" || { echo "FAIL: working-dir not honored"; cat "$TMP/6.log"; fails=$((fails+1)); }

if [ "$fails" -eq 0 ]; then echo "PASS: rust entrypoint"; else echo "FAILED: $fails check(s)"; exit 1; fi
