#!/usr/bin/env bash
# Mock-PATH test for the osv-scanner plugin entrypoint. Stubs osv-scanner + jq
# and asserts the exit semantics — especially that "No package sources found" is
# a clean pass by default (not a job failure) while real errors stay loud.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0

setup() {
  TMP="$(mktemp -d)"; BIN="$TMP/bin"; WORK="$TMP/work"
  mkdir -p "$BIN" "$WORK"
  # osv-scanner stub: emit OSV_MSG, write the --output report when OSV_WRITE=1,
  # exit OSV_RC.
  cat >"$BIN/osv-scanner" <<'EOF'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do case "$1" in --output) out="$2"; shift 2;; *) shift;; esac; done
[ -n "${OSV_MSG:-}" ] && echo "${OSV_MSG}" >&2
[ "${OSV_WRITE:-0}" = "1" ] && [ -n "$out" ] && echo '{"runs":[{"results":[]}]}' > "$out"
exit "${OSV_RC:-0}"
EOF
  # jq stub: the entrypoint only uses jq for the findings count; 0 is fine.
  cat >"$BIN/jq" <<'EOF'
#!/usr/bin/env bash
echo 0
EOF
  chmod +x "$BIN"/*
}
teardown() { rm -rf "$TMP"; }

run() {
  local want="$1"; shift
  ( cd "$WORK" && env "$@" PATH="$BIN:$PATH" sh "$HERE/entrypoint.sh" >/dev/null 2>&1 )
  local got=$?
  if [ "$got" != "$want" ]; then echo "FAIL[$CASE]: exit $got, want $want"; teardown; exit 1; fi
}
ok() { PASS=$((PASS+1)); }

# ── 1. no package sources → clean pass (exit 0) + empty SARIF synthesized ──
CASE=no-sources-clean; setup
run 0 OSV_RC=128 OSV_MSG="No package sources found, --help for usage information."
[ -s "$WORK/osv-report.sarif" ] || { echo "FAIL[$CASE]: no synthetic SARIF for clean no-sources"; teardown; exit 1; }
grep -q '"name":"osv-scanner"' "$WORK/osv-report.sarif" || { echo "FAIL[$CASE]: SARIF not parser-shaped"; teardown; exit 1; }
teardown; ok

# ── 1b. no sources but osv left a (partial) report → overwrite with valid SARIF ──
CASE=no-sources-overwrite; setup
run 0 OSV_RC=128 OSV_WRITE=1 OSV_MSG="No package sources found"
grep -q '"name":"osv-scanner"' "$WORK/osv-report.sarif" \
  || { echo "FAIL[$CASE]: partial report not overwritten with the clean SARIF"; cat "$WORK/osv-report.sarif"; teardown; exit 1; }
teardown; ok

# ── 2. no sources + fail-on-no-sources=true → exit 2 ──
CASE=no-sources-strict; setup
run 2 PLUGIN_FAIL_ON_NO_SOURCES=true OSV_RC=128 OSV_MSG="No package sources found"
teardown; ok

# ── 3. a real scan error (128 but a different message) → exit 2 ──
CASE=real-error; setup
run 2 OSV_RC=128 OSV_MSG="failed to parse lockfile: broken"
teardown; ok

# ── 4. vulnerabilities found (exit 1), default fail-on=any → exit 1 ──
CASE=vulns-fail; setup
run 1 OSV_RC=1 OSV_WRITE=1
teardown; ok

# ── 5. vulnerabilities found, fail-on=none → report-only exit 0 ──
CASE=vulns-advisory; setup
run 0 PLUGIN_FAIL_ON=none OSV_RC=1 OSV_WRITE=1
teardown; ok

# ── 6. clean scan with sources (exit 0) → 0 ──
CASE=clean; setup
run 0 OSV_RC=0 OSV_WRITE=1
teardown; ok

# ── 7. invalid fail-on-no-sources → exit 2 ──
CASE=bad-flag; setup
run 2 PLUGIN_FAIL_ON_NO_SOURCES=maybe
teardown; ok

echo "PASS: osv-scanner entrypoint ($PASS groups)"
