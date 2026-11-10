#!/usr/bin/env bash
# Unit test for plugins/lighthouse-ci/entrypoint.sh — proves the self-hosted
# LHCI build token (PLUGIN_UPLOAD_TOKEN) is handed to `lhci upload` through the
# LHCI_TOKEN env var and NEVER through argv or the `==> lhci upload ...` log
# line (#339). Mode 755 so the plugins.yml pre-build gate runs it (it executes
# entrypoint_test.sh only when [ -x ]).
#
# A mock `lhci` on PATH records, per sub-command, its argv one-per-line to a
# private harness file, and — for `upload` — whether/what LHCI_TOKEN it saw in
# its own environment. The token value is written ONLY to that private file,
# never to the test's stdout.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

# A token value distinctive enough that a stray copy anywhere is unmistakable.
TOKEN_SENTINEL="SENTINELlhciBUILDtoken9b3f"

# --- mock lhci -------------------------------------------------------------
# $TMP/mock holds <sub>.argv (one ARG= line per argument) and, for upload,
# upload.lhci_token (the token the child actually received via env).
mkdir -p "$TMP/mock"
cat >"$TMP/lhci" <<'MOCK'
#!/usr/bin/env bash
sub="${1:-}"; shift || true
dir="${MOCK_DIR:?MOCK_DIR unset}"
: >"$dir/${sub}.argv"
for a in "$@"; do printf 'ARG=%s\n' "$a" >>"$dir/${sub}.argv"; done
if [ "$sub" = upload ]; then
    # Record the token the wrapper passed through the environment, to a file
    # the parent process never echoes. "<<unset>>" distinguishes "not wired".
    printf '%s\n' "${LHCI_TOKEN-<<unset>>}" >"$dir/upload.lhci_token"
fi
exit 0
MOCK
chmod +x "$TMP/lhci"

# run <stdout-file>: invoke the entrypoint with the mock on PATH, capturing its
# stdout+stderr. Env for the specific case comes from the caller via `env`.
# Returns the entrypoint's exit code in RC.
run() {
    local out="$1"; shift
    set +e
    env MOCK_DIR="$TMP/mock" PATH="$TMP:$PATH" "$@" \
        bash "$HERE/entrypoint.sh" >"$out" 2>&1
    RC=$?
    set -e
}

# ---------------------------------------------------------------------------
# 1. upload-target=lhci with a token: token rides LHCI_TOKEN env, never argv,
#    never the echoed log line.
# ---------------------------------------------------------------------------
rm -f "$TMP/mock/upload.argv" "$TMP/mock/upload.lhci_token"
run "$TMP/case1.log" \
    PLUGIN_UPLOAD_TARGET=lhci \
    PLUGIN_UPLOAD_SERVER_BASE_URL=https://lhci.example.com \
    PLUGIN_UPLOAD_TOKEN="$TOKEN_SENTINEL"
[ "$RC" -eq 0 ] || fail "lhci upload should succeed with the mock (rc=$RC); $(cat "$TMP/case1.log")"

[ -f "$TMP/mock/upload.argv" ] || fail "mock lhci upload was never invoked"

# The exact upload vector: --target and --serverBaseUrl present as flags...
grep -qx 'ARG=--target=lhci' "$TMP/mock/upload.argv" \
    || fail "upload argv missing --target=lhci: $(cat "$TMP/mock/upload.argv")"
grep -qx 'ARG=--serverBaseUrl=https://lhci.example.com' "$TMP/mock/upload.argv" \
    || fail "upload argv missing --serverBaseUrl flag: $(cat "$TMP/mock/upload.argv")"

# ...and NO --token flag of any form, and the sentinel on no argv line.
grep -q -- '--token' "$TMP/mock/upload.argv" \
    && fail "token leaked onto argv as a --token flag: $(cat "$TMP/mock/upload.argv")"
grep -qF "$TOKEN_SENTINEL" "$TMP/mock/upload.argv" \
    && fail "token sentinel present on upload argv: $(cat "$TMP/mock/upload.argv")"

# The sentinel must not appear anywhere in the wrapper's own stdout/stderr
# (the `==> lhci upload ...` banner is built from upload_args, which no longer
# holds the token).
grep -qF "$TOKEN_SENTINEL" "$TMP/case1.log" \
    && fail "token sentinel leaked into the wrapper log (==> lhci upload line?)"

# The channel is actually wired: the child saw the token via LHCI_TOKEN.
[ "$(cat "$TMP/mock/upload.lhci_token")" = "$TOKEN_SENTINEL" ] \
    || fail "LHCI_TOKEN did not reach the lhci child (got '$(cat "$TMP/mock/upload.lhci_token")')"
echo "OK  1: lhci token rides LHCI_TOKEN env, absent from argv and log"

# ---------------------------------------------------------------------------
# 2. a non-lhci target must NOT export LHCI_TOKEN even if a token is set — the
#    env channel is scoped to the lhci target only (no ambient-token surprise).
# ---------------------------------------------------------------------------
rm -f "$TMP/mock/upload.argv" "$TMP/mock/upload.lhci_token"
run "$TMP/case2.log" \
    PLUGIN_UPLOAD_TARGET=filesystem \
    PLUGIN_UPLOAD_TOKEN="$TOKEN_SENTINEL"
[ "$RC" -eq 0 ] || fail "filesystem upload should succeed (rc=$RC); $(cat "$TMP/case2.log")"
[ "$(cat "$TMP/mock/upload.lhci_token")" = "<<unset>>" ] \
    || fail "LHCI_TOKEN was exported for a non-lhci target (got '$(cat "$TMP/mock/upload.lhci_token")')"
grep -qx 'ARG=--target=filesystem' "$TMP/mock/upload.argv" \
    || fail "filesystem upload argv wrong: $(cat "$TMP/mock/upload.argv")"
echo "OK  2: non-lhci target does not export LHCI_TOKEN"

# ---------------------------------------------------------------------------
# 3. validation: upload-target=lhci without a server base URL exits 2 before
#    upload runs.
# ---------------------------------------------------------------------------
rm -f "$TMP/mock/upload.argv"
run "$TMP/case3.log" \
    PLUGIN_UPLOAD_TARGET=lhci \
    PLUGIN_UPLOAD_TOKEN="$TOKEN_SENTINEL"
[ "$RC" -eq 2 ] || fail "lhci without server-base-url should exit 2, got $RC"
[ -f "$TMP/mock/upload.argv" ] && fail "upload ran despite missing server-base-url"
grep -qF "$TOKEN_SENTINEL" "$TMP/case3.log" \
    && fail "token sentinel leaked into the validation-error output"
echo "OK  3: lhci without server-base-url exits 2, no token leak"

# ---------------------------------------------------------------------------
# 4. validation: an unknown upload-target exits 2.
# ---------------------------------------------------------------------------
rm -f "$TMP/mock/upload.argv"
run "$TMP/case4.log" PLUGIN_UPLOAD_TARGET=bogus-target
[ "$RC" -eq 2 ] || fail "unknown upload-target should exit 2, got $RC"
[ -f "$TMP/mock/upload.argv" ] && fail "upload ran for an unknown target"
echo "OK  4: unknown upload-target exits 2"

echo "PASS: lighthouse-ci entrypoint unit"
