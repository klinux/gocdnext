#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin" "$TMP/work/coverage" "$TMP/work/sub"
printf 'TN:\nSF:x.js\nDA:1,1\nend_of_record\n' >"$TMP/work/coverage/lcov.info"
printf '{}' >"$TMP/work/sub/report.json"

cat >"$TMP/bin/coveralls" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >"$CALL_FILE"
printf '%s\n' "${COVERALLS_REPO_TOKEN:-}" >"$TOKEN_FILE"
printf '%s\n' "${COVERALLS_SERVICE_NUMBER:-}|${COVERALLS_SERVICE_JOB_ID:-}|${COVERALLS_GIT_COMMIT:-}|${COVERALLS_GIT_BRANCH:-}|${COVERALLS_ENDPOINT:-}" >"$ENV_FILE"
STUB
printf '#!/usr/bin/env bash\nexit 0\n' >"$TMP/bin/git"
chmod +x "$TMP/bin/coveralls" "$TMP/bin/git"

OUT=""; RC=0
run() {
    : >"$TMP/call"; : >"$TMP/token"; : >"$TMP/env"
    OUT="$(cd "$TMP/work" && env -i \
        PATH="$TMP/bin:/usr/bin:/bin" \
        CALL_FILE="$TMP/call" TOKEN_FILE="$TMP/token" ENV_FILE="$TMP/env" \
        "$@" bash "$HERE/entrypoint.sh" 2>&1)"
    RC=$?
}
fail() { echo "FAIL: $1"; echo "--- output ---"; echo "$OUT"; echo "--- call ---"; cat "$TMP/call"; exit 1; }

run PLUGIN_TOKEN=secret CI_RUN_COUNTER=42 CI_RUN_ID=run-id CI_COMMIT_SHA=abc CI_COMMIT_BRANCH=main
[ "$RC" -eq 0 ] || fail "default upload should pass"
grep -qx 'report coverage/lcov.info --format=lcov' "$TMP/call" || fail "default command mismatch"
grep -qx 'secret' "$TMP/token" || fail "token must reach reporter through env"
grep -qx '42|run-id|abc|main|' "$TMP/env" || fail "CI metadata mapping mismatch"
case "$(cat "$TMP/call")" in *secret*) fail "token leaked into argv" ;; esac

run PLUGIN_TOKEN=secret PLUGIN_WORKING_DIR=sub PLUGIN_FILE=report.json PLUGIN_FORMAT=json PLUGIN_BASE_URL=https://coveralls.example/
[ "$RC" -eq 0 ] || fail "working-dir/json upload should pass"
grep -qx 'report report.json --format=coveralls' "$TMP/call" || fail "json must map to coveralls format"
grep -qx '||||https://coveralls.example' "$TMP/env" || fail "enterprise endpoint mismatch"

# Finalise is entry-less: no coverage file lookup, official done command.
rm -f "$TMP/work/coverage/lcov.info"
run PLUGIN_TOKEN=secret PLUGIN_FINALISE=true CI_RUN_COUNTER=7
[ "$RC" -eq 0 ] || fail "finalise should not require a coverage file"
grep -qx 'done --build-number=7' "$TMP/call" || fail "finalise command mismatch"

run PLUGIN_TOKEN=secret PLUGIN_PARALLEL=true
[ "$RC" -eq 2 ] || fail "parallel without run counter must fail clearly"

printf '{}' >"$TMP/work/report"
run PLUGIN_TOKEN=secret PLUGIN_FILE=report PLUGIN_FORMAT=typo
[ "$RC" -eq 2 ] || fail "unsupported format must fail before upload"

run
[ "$RC" -eq 2 ] || fail "missing token must fail"

echo "PASS: coveralls entrypoint"
