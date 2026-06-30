#!/usr/bin/env bash
# Mock-PATH test for the nuclei plugin entrypoint. Stubs nuclei/curl/yq/jq and
# asserts the argv + the security guards (preflight, validation, spec rewrite,
# hardened flags, exit semantics) — no real nuclei/network needed.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0

setup() {
  TMP="$(mktemp -d)"
  BIN="$TMP/bin"; WORK="$TMP/work"
  mkdir -p "$BIN" "$WORK"
  # curl stub: print CURL_CODE (default 200) as http_code, exit CURL_RC
  # (default 0). A real connection failure prints "000" AND exits non-zero —
  # set CURL_CODE=000 CURL_RC=7 to exercise that.
  cat >"$BIN/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s' "${CURL_CODE:-200}"
exit "${CURL_RC:-0}"
EOF
  # nuclei stub: record argv; optionally write SARIF + JSONL; exit NUCLEI_RC.
  cat >"$BIN/nuclei" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$NUCLEI_ARGS"
# capture -sarif-export / -jsonl-export paths
out=""; jsonl=""
while [ $# -gt 0 ]; do
  case "$1" in
    -sarif-export) out="$2"; shift 2;;
    -jsonl-export) jsonl="$2"; shift 2;;
    *) shift;;
  esac
done
[ "${NUCLEI_WRITE_SARIF:-0}" = "1" ] && [ -n "$out" ] && echo '{"runs":[]}' > "$out"
[ -n "${NUCLEI_JSONL:-}" ] && [ -n "$jsonl" ] && printf '%s' "$NUCLEI_JSONL" > "$jsonl"
exit "${NUCLEI_RC:-0}"
EOF
  # yq stub: -e '.openapi'/'.swagger' detect via grep; -i records the expr.
  cat >"$BIN/yq" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-e" ]; then
  key="$(echo "$2" | tr -d '.')"; file="$3"
  grep -qiE "^\s*${key}\s*:" "$file"; exit $?
fi
if [ "$1" = "-i" ]; then printf '%s\n' "$2" >> "$YQ_EXPRS"; exit 0; fi
exit 0
EOF
  # jq stub: extract .info.severity values from the JSONL via grep/sed.
  cat >"$BIN/jq" <<'EOF'
#!/usr/bin/env bash
file="${@: -1}"
grep -oE '"severity":"[a-z]+"' "$file" 2>/dev/null | sed 's/.*:"//; s/"//'
EOF
  chmod +x "$BIN"/*
  NUCLEI_ARGS="$TMP/args"; YQ_EXPRS="$TMP/yq"
  : > "$YQ_EXPRS"
}
teardown() { rm -rf "$TMP"; }

# run <expected_rc> <env assignments...> -- runs the entrypoint in $WORK.
run() {
  local want="$1"; shift
  ( cd "$WORK" && env "$@" NUCLEI_ARGS="$NUCLEI_ARGS" YQ_EXPRS="$YQ_EXPRS" \
      PATH="$BIN:$PATH" bash "$HERE/entrypoint.sh" >/dev/null 2>&1 )
  local got=$?
  if [ "$got" != "$want" ]; then echo "FAIL[$CASE]: exit $got, want $want"; [ -f "$NUCLEI_ARGS" ] && { echo "--args--"; cat "$NUCLEI_ARGS"; }; teardown; exit 1; fi
}
has() { grep -qx -- "$1" "$NUCLEI_ARGS" || { echo "FAIL[$CASE]: missing arg $1"; cat "$NUCLEI_ARGS"; teardown; exit 1; }; }
hasnt() { grep -qx -- "$1" "$NUCLEI_ARGS" && { echo "FAIL[$CASE]: unexpected arg $1"; teardown; exit 1; }; return 0; }
ok() { PASS=$((PASS+1)); }

# ── 1. missing target → exit 2 ──
CASE=missing-target; setup
run 2 PLUGIN_TARGET=
teardown; ok

# ── 2. baseline → hardened flags + -u target, exit 0 ──
CASE=baseline; setup
run 0 PLUGIN_TARGET=http://app:8080
has "-u"; has "http://app:8080"; has "-severity"; has "-omit-raw"; has "-redact"
has "-disable-update-check"; has "-disable-redirects"; has "-type"; has "http"
has "-no-interactsh"; has "-t"; has "/opt/nuclei-templates"
teardown; ok

# ── 3. SARIF synthesized on a clean run (nuclei wrote none) ──
CASE=sarif-clean; setup
run 0 PLUGIN_TARGET=http://app:8080
[ -s "$WORK/nuclei.sarif" ] || { echo "FAIL[$CASE]: no SARIF on clean run"; teardown; exit 1; }
grep -q '"name":"nuclei"' "$WORK/nuclei.sarif" || { echo "FAIL[$CASE]: SARIF not parser-shaped"; teardown; exit 1; }
teardown; ok

# ── 4. fail-on ⊄ severity → exit 2 ──
CASE=failon-subset; setup
run 2 PLUGIN_TARGET=http://app:8080 PLUGIN_SEVERITY=medium PLUGIN_FAIL_ON=critical
teardown; ok

# ── 5. interactsh-server without interactsh:true → exit 2 ──
CASE=interactsh-guard; setup
run 2 PLUGIN_TARGET=http://app:8080 PLUGIN_INTERACTSH_SERVER=https://oast.example
teardown; ok

# ── 6. bad numeric inputs → exit 2 ──
CASE=bad-ints; setup
run 2 PLUGIN_TARGET=http://app:8080 PLUGIN_RATE_LIMIT=abc
setup; run 2 PLUGIN_TARGET=http://app:8080 PLUGIN_EXIT_CODE=7
teardown; ok

# ── 7. preflight 000 → exit 2, no SARIF ──
CASE=preflight-down; setup
run 2 PLUGIN_TARGET=http://app:8080 PLUGIN_READY_TIMEOUT=1 CURL_CODE=000 CURL_RC=7
[ -e "$WORK/nuclei.sarif" ] && { echo "FAIL[$CASE]: SARIF written for unreachable target"; teardown; exit 1; }
teardown; ok

# ── 8. interactsh:true → no -no-interactsh ──
CASE=interactsh-on; setup
run 0 PLUGIN_TARGET=http://app:8080 PLUGIN_INTERACTSH=true
hasnt "-no-interactsh"
teardown; ok

# ── 9. templates override replaces the baked default ──
CASE=templates-override; setup
run 0 PLUGIN_TARGET=http://app:8080 PLUGIN_TEMPLATES=/custom/tmpl
has "/custom/tmpl"; hasnt "/opt/nuclei-templates"
teardown; ok

# ── 10. openapi spec → -im openapi + servers rewrite + -list ──
CASE=spec-openapi; setup
printf 'openapi: 3.0.0\nservers:\n  - url: https://prod.example\n' > "$WORK/api.yaml"
run 0 PLUGIN_TARGET=http://app:8080 PLUGIN_SPEC=api.yaml
has "-im"; has "openapi"; has "-list"; hasnt "-u"
grep -q 'servers' "$YQ_EXPRS" || { echo "FAIL[$CASE]: servers not rewritten"; cat "$YQ_EXPRS"; teardown; exit 1; }
grep -q 'strenv(TARGET)' "$YQ_EXPRS" || { echo "FAIL[$CASE]: target not used in rewrite"; teardown; exit 1; }
# The rewrite must neutralize NESTED servers too (path/operation level), not
# just the root — else an OpenAPI shipping `paths./x.get.servers: prod` slips
# the scan to prod. Assert the recursive marker, and (when a real yq is present)
# run the entrypoint's ACTUAL recorded expr against a nested spec → 0 prod left.
grep -q 'has("servers")' "$YQ_EXPRS" || { echo "FAIL[$CASE]: rewrite isn't recursive (nested servers would leak)"; cat "$YQ_EXPRS"; teardown; exit 1; }
if command -v yq >/dev/null 2>&1; then
  expr="$(grep 'servers' "$YQ_EXPRS" | head -1)"
  printf 'openapi: 3.0.0\nservers: [{url: https://prod-root}]\npaths:\n  /x: {servers: [{url: https://prod-path}], get: {servers: [{url: https://prod-op}]}}\n' > "$WORK/nested.yaml"
  TARGET=http://app:8080 yq -i "$expr" "$WORK/nested.yaml"
  grep -q 'prod' "$WORK/nested.yaml" && { echo "FAIL[$CASE]: real-yq rewrite left a prod server (nested leak)"; cat "$WORK/nested.yaml"; teardown; exit 1; }
fi
teardown; ok

# ── 11. swagger spec → -im swagger + scheme/host/basePath rewrite ──
CASE=spec-swagger; setup
printf 'swagger: "2.0"\nhost: prod.example\nbasePath: /\n' > "$WORK/sw.yaml"
run 0 PLUGIN_TARGET=http://app:8080/api PLUGIN_SPEC=sw.yaml
has "-im"; has "swagger"
grep -q '.schemes' "$YQ_EXPRS" && grep -q '.host' "$YQ_EXPRS" && grep -q '.basePath' "$YQ_EXPRS" \
  || { echo "FAIL[$CASE]: swagger fields not rewritten"; cat "$YQ_EXPRS"; teardown; exit 1; }
teardown; ok

# ── 12. nuclei operational error (non-zero) → exit 2, no synthetic SARIF ──
CASE=nuclei-error; setup
run 2 PLUGIN_TARGET=http://app:8080 NUCLEI_RC=1
[ -e "$WORK/nuclei.sarif" ] && { echo "FAIL[$CASE]: SARIF synthesized on nuclei error"; teardown; exit 1; }
teardown; ok

# ── 13. findings at/above fail-on → exit-code; advisory → 0 ──
CASE=findings-fail; setup
run 1 PLUGIN_TARGET=http://app:8080 NUCLEI_JSONL='{"info":{"severity":"high"}}'
setup; CASE=findings-advisory
run 0 PLUGIN_TARGET=http://app:8080 PLUGIN_EXIT_CODE=0 NUCLEI_JSONL='{"info":{"severity":"high"}}'
setup; CASE=findings-below
run 0 PLUGIN_TARGET=http://app:8080 NUCLEI_JSONL='{"info":{"severity":"low"}}'
# threshold (at/above): fail-on=medium must also catch a critical finding.
setup; CASE=findings-threshold
run 1 PLUGIN_TARGET=http://app:8080 PLUGIN_FAIL_ON=medium NUCLEI_JSONL='{"info":{"severity":"critical"}}'
teardown; ok

# ── 14. health-path join is clean ──
CASE=health-join; setup
cat >"$BIN/curl" <<'EOF'
#!/usr/bin/env bash
echo "${@: -1}" >> "$CURL_URLS"; printf '200'
EOF
chmod +x "$BIN/curl"
CURL_URLS="$TMP/urls"; : > "$CURL_URLS"
run 0 PLUGIN_TARGET=http://app:8080/api PLUGIN_HEALTH_PATH=/health CURL_URLS="$CURL_URLS"
grep -qx 'http://app:8080/api/health' "$CURL_URLS" || { echo "FAIL[$CASE]: bad preflight url"; cat "$CURL_URLS"; teardown; exit 1; }
teardown; ok

echo "PASS: nuclei entrypoint ($PASS groups)"
