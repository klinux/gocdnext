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
# Append (not overwrite) so a second invocation — the failure-summary re-run —
# is recorded too. Exit code is controllable via TRIVY_MOCK_EXIT (default 0).
printf '%s\n' "$@" >> "$TRIVY_ARGS_FILE"
exit "${TRIVY_MOCK_EXIT:-0}"
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

# ── 3. scan_type=config uses the binary's EMBEDDED checks (--skip-check-update)
#       so a newer downloaded rego bundle can't skew against the pinned binary ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=config run || fail "entrypoint failed for scan_type=config"
grep -qx -- "--skip-check-update" "$TMP/args" || fail "config scan missing --skip-check-update"

# ── 4. non-config scans do NOT pass --skip-check-update (no misconfig checks) ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=fs run || fail "entrypoint failed for scan_type=fs"
grep -qx -- "--skip-check-update" "$TMP/args" && fail "spurious --skip-check-update on a non-config scan"

# ── 5. a machine-format gate FAILURE re-prints findings as a human table so the
#       real cause is visible in the log (not just the exit code) ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=config PLUGIN_FORMAT=sarif PLUGIN_OUTPUT=out.sarif TRIVY_MOCK_EXIT=1 run
rc=$?
[ "$rc" -eq 1 ] || fail "entrypoint must propagate the gate's non-zero exit (got $rc)"
[ "$(grep -cx -- config "$TMP/args")" -eq 2 ] || fail "expected a second (summary) trivy run on failure"
grep -qx -- "table" "$TMP/args" || fail "summary run should use --format table"

# ── 6. a clean scan (exit 0) does NOT trigger the summary re-run ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=config PLUGIN_FORMAT=sarif PLUGIN_OUTPUT=out.sarif TRIVY_MOCK_EXIT=0 run \
  || fail "clean scan should exit 0"
[ "$(grep -cx -- config "$TMP/args")" -eq 1 ] || fail "no summary run expected on success"

# ── 7. a failure whose primary format is ALREADY a table doesn't re-run ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=config PLUGIN_FORMAT=table TRIVY_MOCK_EXIT=1 run
rc=$?
[ "$rc" -eq 1 ] || fail "table-format gate failure should still exit 1 (got $rc)"
[ "$(grep -cx -- config "$TMP/args")" -eq 1 ] || fail "no summary re-run when the primary is already a table"

# ── 8. offline runner: --skip-db-update rides BOTH the primary and the summary
#       re-run, so the failure table doesn't try to touch the network ──
rm -f "$TMP/args"
PLUGIN_SCAN_TYPE=config PLUGIN_FORMAT=sarif PLUGIN_OUTPUT=out.sarif \
  PLUGIN_SKIP_DB_UPDATE=true TRIVY_MOCK_EXIT=1 run
rc=$?
[ "$rc" -eq 1 ] || fail "offline gate failure should still exit 1 (got $rc)"
[ "$(grep -cx -- "--skip-db-update" "$TMP/args")" -eq 2 ] \
  || fail "--skip-db-update must ride both the primary AND the summary re-run"

echo "PASS: trivy entrypoint"
