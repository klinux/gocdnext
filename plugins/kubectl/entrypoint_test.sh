#!/usr/bin/env bash
# Mock-PATH unit test for the kubectl plugin entrypoint. No bats — a plain
# bash harness that stubs `kubectl` (exit code driven by MOCK_RC) and asserts
# the entrypoint's own exit code + that continue_on_error downgrades a failure
# to a warning without failing the run (#303).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub kubectl: log argv, print a marker to stdout, exit with ${MOCK_RC:-0}.
cat >"$TMP/kubectl" <<'EOF'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"$TMP_LOG"
echo "MOCK_KUBECTL_RAN"
exit "${MOCK_RC:-0}"
EOF
chmod +x "$TMP/kubectl"

fails=0
# run <expected_rc> <desc> -- runs the entrypoint with the mock on PATH and the
# current env; asserts the entrypoint's exit code. Captures combined output in
# $OUT for content assertions.
run() {
    local want="$1" desc="$2"; shift 3 # drop the literal `--`
    TMP_LOG="$TMP/kubectl.log" OUT="$(PATH="$TMP:$PATH" TMP_LOG="$TMP/kubectl.log" bash "$HERE/entrypoint.sh" 2>&1)"
    local got=$?
    if [ "$got" != "$want" ]; then
        echo "FAIL: $desc — exit $got, want $want"; echo "  out: $OUT"; fails=1
    else
        echo "PASS: $desc (exit $got)"
    fi
}

echo "== 1. success passes through (exit 0) =="
MOCK_RC=0 PLUGIN_COMMAND="get pods" run 0 "success → 0" --

echo "== 2. failure WITHOUT continue_on_error propagates =="
MOCK_RC=1 PLUGIN_COMMAND="apply -f k8s/" run 1 "failure → 1 (propagates)" --

echo "== 3. failure WITH continue_on_error → 0 + WARNING =="
MOCK_RC=1 PLUGIN_CONTINUE_ON_ERROR=true PLUGIN_COMMAND="logs job/migration-api" \
    run 0 "failure + continue_on_error → 0" --
case "$OUT" in
    *"continue_on_error is set"*"NOT failing the run"*) echo "PASS: warning printed" ;;
    *) echo "FAIL: expected warning about continue_on_error; got: $OUT"; fails=1 ;;
esac

echo "== 4. continue_on_error but command SUCCEEDS → 0, no warning =="
MOCK_RC=0 PLUGIN_CONTINUE_ON_ERROR=true PLUGIN_COMMAND="logs job/migration-api" \
    run 0 "success + continue_on_error → 0" --
case "$OUT" in
    *"NOT failing the run"*) echo "FAIL: warning printed on success"; fails=1 ;;
    *) echo "PASS: no spurious warning on success" ;;
esac

echo "== 5. continue_on_error accepts 1/yes/on, ignores garbage =="
MOCK_RC=1 PLUGIN_CONTINUE_ON_ERROR=1   PLUGIN_COMMAND="logs x" run 0 "'1' → tolerant" --
MOCK_RC=1 PLUGIN_CONTINUE_ON_ERROR=ON  PLUGIN_COMMAND="logs x" run 0 "'ON' → tolerant" --
MOCK_RC=1 PLUGIN_CONTINUE_ON_ERROR=nope PLUGIN_COMMAND="logs x" run 1 "'nope' → still fails" --

echo "== 6. missing PLUGIN_COMMAND → exit 2 (unchanged) =="
PLUGIN_COMMAND="" run 2 "no command → 2" --

if [ "$fails" = 0 ]; then echo "ALL PASS"; else echo "SOME FAILED"; exit 1; fi
