#!/usr/bin/env bash
# Unit test for plugins/codecov/entrypoint.sh — proves the Codecov upload token
# (PLUGIN_TOKEN) reaches the `codecov` child through the CODECOV_TOKEN env var and
# NEVER through argv or the `==> codecov ...` log line (#393), and that the secret
# is not carried under two names (PLUGIN_TOKEN is unset after the export). Mode 755
# so the plugins.yml pre-build gate runs it (it executes entrypoint_test.sh only
# when [ -x ]).
#
# A mock `codecov` on PATH records its argv one-per-line to a private harness file
# and — to a separate private file — the CODECOV_TOKEN it saw in its own env and
# whether PLUGIN_TOKEN was still set. Those values are written ONLY to those
# private files, never to the test's stdout. The entrypoint `exec`s codecov, so the
# mock is the final process; it exits 0 to stand in for a successful upload.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

# A token value distinctive enough that a stray copy anywhere is unmistakable.
TOKEN_SENTINEL="SENTINELcodecovTOKEN7a2e"

# --- mock codecov ----------------------------------------------------------
# $TMP/mock holds codecov.argv (one ARG= line per argument), codecov.token (the
# CODECOV_TOKEN the child received), and codecov.plugin_token (PLUGIN_TOKEN as the
# child saw it — "<<unset>>" proves the wrapper unset it).
mkdir -p "$TMP/mock"
cat >"$TMP/codecov" <<'MOCK'
#!/usr/bin/env bash
dir="${MOCK_DIR:?MOCK_DIR unset}"
: >"$dir/codecov.argv"
for a in "$@"; do printf 'ARG=%s\n' "$a" >>"$dir/codecov.argv"; done
printf '%s\n' "${CODECOV_TOKEN-<<unset>>}"  >"$dir/codecov.token"
printf '%s\n' "${PLUGIN_TOKEN-<<unset>>}"   >"$dir/codecov.plugin_token"
exit 0
MOCK
chmod +x "$TMP/codecov"

# run <stdout-file> ENV...: invoke the entrypoint with the mock first on PATH,
# capturing stdout+stderr. Case-specific env comes from the caller via `env`.
# Returns the entrypoint's exit code in RC.
#
# Start from a clean slate: `-u` strips every modeled input (PLUGIN_* and
# CODECOV_TOKEN) from the inherited environment BEFORE the case-specific
# assignments are applied, so a value in the harness's own environment can't leak
# in — otherwise the "no token" case would silently pass with an ambient
# PLUGIN_TOKEN and stop testing anything. Case vars in "$@" come after and re-add
# exactly what each case needs.
run() {
    local out="$1"; shift
    set +e
    env -u PLUGIN_TOKEN -u PLUGIN_FILE -u PLUGIN_FLAGS -u PLUGIN_SLUG \
        -u PLUGIN_URL -u PLUGIN_WORKING_DIR -u CODECOV_TOKEN \
        MOCK_DIR="$TMP/mock" PATH="$TMP:$PATH" "$@" \
        bash "$HERE/entrypoint.sh" >"$out" 2>&1
    RC=$?
    set -e
}

reset_mock() { rm -f "$TMP/mock/codecov.argv" "$TMP/mock/codecov.token" "$TMP/mock/codecov.plugin_token"; }

# ---------------------------------------------------------------------------
# 1. Full option set: token rides CODECOV_TOKEN, every other input becomes a
#    flag, and the token appears on NO argv line and NOT in the banner/log.
# ---------------------------------------------------------------------------
reset_mock
run "$TMP/case1.log" \
    PLUGIN_TOKEN="$TOKEN_SENTINEL" \
    PLUGIN_FILE=cover.out \
    PLUGIN_FLAGS="unit,integration" \
    PLUGIN_SLUG=acme/repo \
    PLUGIN_URL=https://codecov.example.com
[ "$RC" -eq 0 ] || fail "codecov upload should succeed with the mock (rc=$RC); $(cat "$TMP/case1.log")"

[ -f "$TMP/mock/codecov.argv" ] || fail "mock codecov was never invoked"

# The upload vector is assembled as expected...
grep -qx 'ARG=upload-process'          "$TMP/mock/codecov.argv" || fail "argv missing upload-process: $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=--file'                  "$TMP/mock/codecov.argv" || fail "argv missing --file: $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=cover.out'               "$TMP/mock/codecov.argv" || fail "argv missing file value: $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=--flag'                  "$TMP/mock/codecov.argv" || fail "argv missing --flag: $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=unit'                    "$TMP/mock/codecov.argv" || fail "argv missing flag 'unit': $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=integration'             "$TMP/mock/codecov.argv" || fail "argv missing flag 'integration': $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=--slug'                  "$TMP/mock/codecov.argv" || fail "argv missing --slug: $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=--url'                   "$TMP/mock/codecov.argv" || fail "argv missing --url: $(cat "$TMP/mock/codecov.argv")"

# ...with NO --token flag of any form, and the sentinel on no argv line.
grep -q -- '--token' "$TMP/mock/codecov.argv" \
    && fail "token leaked onto argv as a --token flag: $(cat "$TMP/mock/codecov.argv")"
grep -qF "$TOKEN_SENTINEL" "$TMP/mock/codecov.argv" \
    && fail "token sentinel present on codecov argv: $(cat "$TMP/mock/codecov.argv")"

# The sentinel must not appear anywhere in the wrapper's own stdout/stderr
# (the `==> codecov ...` banner is built from args, which no longer holds it).
grep -qF "$TOKEN_SENTINEL" "$TMP/case1.log" \
    && fail "token sentinel leaked into the wrapper log (==> codecov line?)"

# The channel is actually wired: the child saw the token via CODECOV_TOKEN...
[ "$(cat "$TMP/mock/codecov.token")" = "$TOKEN_SENTINEL" ] \
    || fail "CODECOV_TOKEN did not reach the codecov child (got '$(cat "$TMP/mock/codecov.token")')"
# ...and NOT also under the original name — the wrapper unset PLUGIN_TOKEN.
[ "$(cat "$TMP/mock/codecov.plugin_token")" = "<<unset>>" ] \
    || fail "PLUGIN_TOKEN still set in the child — secret carried under two names (got '$(cat "$TMP/mock/codecov.plugin_token")')"
echo "OK  1: token rides CODECOV_TOKEN only, absent from argv and log; PLUGIN_TOKEN unset"

# ---------------------------------------------------------------------------
# 2. Minimal input (token only): argv is exactly `upload-process`, token wired,
#    banner clean.
# ---------------------------------------------------------------------------
reset_mock
run "$TMP/case2.log" PLUGIN_TOKEN="$TOKEN_SENTINEL"
[ "$RC" -eq 0 ] || fail "minimal upload should succeed (rc=$RC); $(cat "$TMP/case2.log")"
[ "$(wc -l <"$TMP/mock/codecov.argv")" -eq 1 ] \
    || fail "minimal argv should be exactly one arg: $(cat "$TMP/mock/codecov.argv")"
grep -qx 'ARG=upload-process' "$TMP/mock/codecov.argv" || fail "minimal argv is not upload-process: $(cat "$TMP/mock/codecov.argv")"
[ "$(cat "$TMP/mock/codecov.token")" = "$TOKEN_SENTINEL" ] \
    || fail "CODECOV_TOKEN not wired on the minimal path (got '$(cat "$TMP/mock/codecov.token")')"
grep -qF "$TOKEN_SENTINEL" "$TMP/case2.log" && fail "token sentinel leaked into the minimal-path log"
echo "OK  2: minimal path is bare upload-process, token via env, log clean"

# ---------------------------------------------------------------------------
# 3. Negative control: no PLUGIN_TOKEN -> exit 2 before codecov runs, no leak.
# ---------------------------------------------------------------------------
reset_mock
run "$TMP/case3.log"
[ "$RC" -eq 2 ] || fail "missing token should exit 2, got $RC"
[ -f "$TMP/mock/codecov.argv" ] && fail "codecov ran despite a missing token"
grep -qi 'token is required' "$TMP/case3.log" || fail "missing-token message not emitted: $(cat "$TMP/case3.log")"
echo "OK  3: missing token exits 2, codecov never runs"

echo "PASS: codecov entrypoint unit"
