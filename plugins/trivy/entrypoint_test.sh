#!/usr/bin/env bash
# Mock-PATH test for the trivy plugin entrypoint. Stubs `trivy` and
# asserts the argv the entrypoint builds — specifically that
# PLUGIN_SKIP_DIRS becomes a `--skip-dirs <value>` flag (and is absent
# when the input is unset).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/trivy" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$TRIVY_ARGS_FILE"
EOF
chmod +x "$TMP/trivy"

run() { ( cd "$TMP" && TRIVY_ARGS_FILE="$TMP/args" PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh" ); }
fail() { echo "FAIL: $1"; [ -f "$TMP/args" ] && { echo "--- args ---"; cat "$TMP/args"; }; exit 1; }

# ── 1. skip_dirs forwarded as a single --skip-dirs argument ──
PLUGIN_SCAN_TYPE=fs PLUGIN_SKIP_DIRS="node_modules,vendor" run || fail "entrypoint failed with skip-dirs"
grep -qx -- "--skip-dirs" "$TMP/args" || fail "missing --skip-dirs flag"
grep -qx -- "node_modules,vendor" "$TMP/args" || fail "skip-dirs value not forwarded as one arg"

# ── 2. absent → no --skip-dirs flag ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=fs run || fail "entrypoint failed without skip-dirs"
[ -f "$TMP/args" ] || fail "trivy was not invoked without skip-dirs"
grep -qx -- "--skip-dirs" "$TMP/args" && fail "spurious --skip-dirs without the input"

echo "PASS: trivy entrypoint"
