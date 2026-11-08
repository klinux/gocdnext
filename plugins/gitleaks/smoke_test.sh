#!/usr/bin/env bash
# Real smoke for the gitleaks plugin's IMAGE. Runs the PINNED binary end to end
# against a throwaway workspace so a bump that renames a subcommand/flag or breaks
# the exit mapping fails HERE, before publish. Proves the source bump
# (v8.21.2 upstream image -> v8.30.1 built on go1.27, alpine 3.24.4):
#   - the binary is v8.30.1;
#   - a workspace WITH a secret is detected: exit-code 1 -> plugin exits 1, the
#     JSON report has findings, and no deprecation warning is printed;
#   - a CLEAN workspace is a pass (exit 0, empty report);
#   - exit-code 0 (advisory) reports findings but exits 0.
#
# Fully offline (gitleaks ships its rules embedded — no network). Convention-named
# so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker. Built --no-cache to
# match the no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-gitleaks:smoke-$$"
WD="$(mktemp -d)"   # dirty workspace (has a secret)
WC="$(mktemp -d)"   # clean workspace
SMOKE_OK=0
cleanup() {
  # gitleaks writes the report as root into the mounts; remove from inside the
  # image (matching uid) before deleting the dirs, then verify they're gone.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$WD:/a" -v "$WC:/b" "$IMG" \
      -c 'rm -rf /a/* /a/.[!.]* /b/* /b/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$WD" "$WC" 2>/dev/null || true
  if [ -d "$WD" ] || [ -d "$WC" ]; then
    echo "WARN: cleanup could not fully remove the workspaces" >&2
    [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left files behind" >&2; exit 1; }
  fi
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }
no_deprecation() { grep -qiE 'deprecat' <<<"$2" && die "$1: used a deprecated gitleaks subcommand/flag" "$2" || true; }
# count findings in a gitleaks JSON report (array of objects with "RuleID") and
# read its raw bytes — the report is root-owned in the mount, so go via the image.
count()  { docker run --rm --network none -v "$1:/w" --entrypoint sh  "$IMG" -c '[ -f "/w/'"$2"'" ] && { grep -c "\"RuleID\"" "/w/'"$2"'" || true; } || echo 0'; }
report() { docker run --rm --network none -v "$1:/w" --entrypoint cat "$IMG" "/w/$2" 2>/dev/null; }
# rmrep must actually remove the report AND verify it's gone — a swallowed failure
# would let a stale report from a prior case satisfy the next one.
rmrep()  { docker run --rm --network none -v "$1:/w" --entrypoint sh "$IMG" \
             -c 'rm -f "/w/'"$2"'" && test ! -e "/w/'"$2"'"' \
             || die "could not remove stale report /w/$2 before a case"; }

# The planted secret VALUES — referenced both when planting and when asserting
# that --redact kept them out of the log and the JSON report.
AWS_SECRET='wJalrXUtnFEMI/K7MDENG/bPxRfiCYz2aBcDeFgH'
KEY_BODY='MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDummykeydata12'
assert_redacted() { # <label> <out> <workspace> <report>
  local lbl="$1" out="$2" ws="$3" rep="$4" rc; rc="$(report "$ws" "$rep")"
  for s in "$AWS_SECRET" "$KEY_BODY"; do
    grep -qF "$s" <<<"$out" && die "${lbl}: raw secret value leaked into the log (redaction off)" "$out"
    grep -qF "$s" <<<"$rc"  && die "${lbl}: raw secret value leaked into the JSON report" "$rc"
  done
  return 0
}

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> gitleaks is v8.30.1"
OUT="$(docker run --rm --network none --entrypoint gitleaks "$IMG" version 2>&1)" || die "gitleaks version failed" "$OUT"
grep -q '8.30.1' <<<"$OUT" || die "gitleaks is not v8.30.1 (got: ${OUT})" "$OUT"

echo "==> stage a workspace WITH a secret"
# A non-allowlisted AWS secret-key-shaped value (NOT the AKIA…EXAMPLE allowlisted
# demo key) + a private key block — both hit gitleaks' default ruleset.
printf 'aws_secret_access_key = "%s"\n' "$AWS_SECRET" >"$WD/creds.env"
printf -- '-----BEGIN PRIVATE KEY-----\n%s\n-----END PRIVATE KEY-----\n' "$KEY_BODY" >"$WD/id.pem"

echo "==> secret detected: exit-code 1 -> plugin exits 1, report has findings, values redacted"
rmrep "$WD" gitleaks.json
set +e
OUT="$(docker run --rm --network none -v "$WD:/workspace" -w /workspace \
  -e PLUGIN_SCAN_MODE=dir -e PLUGIN_EXIT_CODE=1 -e PLUGIN_REPORT=gitleaks.json "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 1 ] || die "detect: expected exit 1, got $RC" "$OUT"
grep -qiE 'leaks found' <<<"$OUT" || die "detect: gitleaks reported no leaks for a planted secret" "$OUT"
no_deprecation "detect" "$OUT"
n="$(count "$WD" gitleaks.json)"
[ -n "$n" ] && [ "$n" -gt 0 ] 2>/dev/null || die "detect: report has no findings (got '${n}')" "$OUT"
assert_redacted "detect" "$OUT" "$WD" gitleaks.json
echo "   (found ${n} findings, redacted)"

echo "==> advisory mode: exit-code 0 still reports findings but passes"
rmrep "$WD" gitleaks.json   # fresh report — never pass on a stale file
set +e
OUT="$(docker run --rm --network none -v "$WD:/workspace" -w /workspace \
  -e PLUGIN_SCAN_MODE=dir -e PLUGIN_EXIT_CODE=0 -e PLUGIN_REPORT=gitleaks.json "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "advisory: expected exit 0, got $RC" "$OUT"
no_deprecation "advisory" "$OUT"
n="$(count "$WD" gitleaks.json)"
[ -n "$n" ] && [ "$n" -gt 0 ] 2>/dev/null || die "advisory: report missing findings — advisory stopped writing (got '${n}')" "$OUT"
assert_redacted "advisory" "$OUT" "$WD" gitleaks.json

echo "==> clean workspace passes (exit 0) with a valid, empty report"
echo 'the quick brown fox' >"$WC/readme.txt"
rmrep "$WC" gitleaks.json
set +e
OUT="$(docker run --rm --network none -v "$WC:/workspace" -w /workspace \
  -e PLUGIN_SCAN_MODE=dir -e PLUGIN_EXIT_CODE=1 -e PLUGIN_REPORT=gitleaks.json "$IMG" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "clean: expected exit 0, got $RC" "$OUT"
no_deprecation "clean" "$OUT"
# Require the report to be PRESENT and a valid, empty array — count==0 alone would
# also pass if no report were written at all.
docker run --rm --network none -v "$WC:/w" --entrypoint sh "$IMG" -c 'test -e /w/gitleaks.json' \
  || die "clean: no report written (expected an empty '[]')" "$OUT"
clean_rep="$(printf '%s' "$(report "$WC" gitleaks.json)" | tr -d '[:space:]')"
[ "$clean_rep" = "[]" ] || die "clean: report is not a valid empty array (got '${clean_rep}')" "$OUT"

SMOKE_OK=1
echo "PASS: gitleaks smoke"
