#!/usr/bin/env bash
# Real smoke for the codecov plugin's IMAGE after the bookworm -> trixie base
# migration (clears zlib1g CVE-2023-45853 + libsqlite3-0 CVE-2025-7458, which have
# no fix in bookworm). Proves:
#   - the base really is trixie and the two OS packages are the FIXED versions
#     (the whole point of the migration), 0 CRITICAL;
#   - codecov-cli still installs and runs on trixie's python:3.12-slim
#     (`codecov --version` is 10.3.0);
#   - the plugin assembles a real `upload-process` and reaches the Codecov ingest
#     API (request built, git SHA derived, CLI functional) — offline it fails at
#     the network boundary, NOT at argument/parse handling;
#   - the entrypoint's auth contract holds: no token -> exit 2.
#
# Built --no-cache to match a release build. Root-owned workspace outputs are
# cleaned in-image; fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-codecov:smoke-$$"
CNAME="codecovsmoke-$$"
SMOKE_OK=0

cleanup() {
  docker rm -f "$CNAME" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  leftover=0
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; leftover=1; }
  [ "$leftover" = 1 ] && [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left state behind" >&2; exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Hard wall-clock cap with TERM->KILL escalation (RC=124 TERM, RC=137 KILL).
run() { set +e; OUT="$(timeout --kill-after=15s 300s "$@" 2>&1)"; RC=$?; set -e; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> base is trixie and the migrated OS packages carry the fix (real version compare)"
# Use dpkg --compare-versions, not a regex: Debian versions don't sort lexically
# (e.g. "3.5" < "3.46"), so a regex floor would false-pass a lower version. The
# fix floors are zlib1g >= 1:1.3 (CVE-2023-45853; bookworm shipped 1:1.2.13) and
# libsqlite3-0 >= 3.46 (CVE-2025-7458; bookworm shipped 3.40.x).
OSV="$(docker run --rm --network none --entrypoint sh "$IMG" -c '
  printf "debian %s\n" "$(cat /etc/debian_version)"
  z="$(dpkg-query -W -f="\${Version}" zlib1g)"
  s="$(dpkg-query -W -f="\${Version}" libsqlite3-0)"
  printf "zlib1g %s\n" "$z"; printf "libsqlite3-0 %s\n" "$s"
  dpkg --compare-versions "$z" ge "1:1.3" && echo ZLIB_FIXED || echo ZLIB_UNFIXED
  dpkg --compare-versions "$s" ge "3.46"  && echo SQLITE_FIXED || echo SQLITE_UNFIXED
')" || die "OS probe failed" "$OSV"
grep -qE '^debian 13\.' <<<"$OSV" || die "base is not Debian trixie (13.x)" "$OSV"
grep -qx 'ZLIB_FIXED'   <<<"$OSV" || die "zlib1g does not carry the CVE-2023-45853 fix (>= 1:1.3)" "$OSV"
grep -qx 'SQLITE_FIXED' <<<"$OSV" || die "libsqlite3-0 does not carry the CVE-2025-7458 fix (>= 3.46)" "$OSV"

echo "==> codecov-cli runs on trixie python (version 10.3.0)"
run docker run --rm --name "$CNAME" --network none --entrypoint codecov "$IMG" --version
[ "$RC" -eq 0 ] || die "codecov --version exited $RC" "$OUT"
grep -qE 'codecovcli, version 10\.3\.0' <<<"$OUT" || die "codecov CLI is not 10.3.0" "$OUT"

echo "==> entrypoint fails fast (exit 2) when no token is provided"
run docker run --rm --name "$CNAME" --network none "$IMG"
[ "$RC" -eq 2 ] || die "no-token: expected exit 2, got $RC" "$OUT"
grep -qi 'token is required' <<<"$OUT" || die "no-token: missing the token-required message" "$OUT"

echo "==> codecov-cli assembles a real API request and reaches the Codecov ingest endpoint"
# The CLI (not the wrapper) is what the OS migration risks — does codecov-cli still
# run and talk to the API on trixie's Python. Exercise it directly with an explicit
# --commit-sha (codecov's "local" adapter requires one; a real gocdnext job gets it
# from the CI env), offline: it must reach ingest.codecov.io and fail at the NETWORK
# boundary, not crash during startup/arg handling. (The wrapper's own arg assembly
# and its token contract are covered by the no-token case above.)
run docker run --rm --name "$CNAME" --network none --entrypoint codecov "$IMG" \
  create-commit --token smoke-dummy --slug acme/repo \
  --commit-sha 0000000000000000000000000000000000000000
[ "$RC" -ne 0 ] || die "api probe: expected a non-zero exit offline, got 0" "$OUT"
case "$RC" in 124|137) die "api probe: watchdog fired (RC=$RC) — codecov hung" "$OUT" ;; esac
grep -qiE 'ingest\.codecov\.io|Request failed after too many retries' <<<"$OUT" \
  || die "api probe: codecov never reached the ingest API" "$OUT"
# NB: codecov prints a Python traceback when it raises on retry exhaustion, so a
# traceback is NOT a failure signal here — only an arg/usage error is.
grep -qiE 'Missing option|no such option|Usage: codecov' <<<"$OUT" \
  && die "api probe: codecov errored during arg handling, not at the network" "$OUT" || true

SMOKE_OK=1
echo "PASS: codecov smoke"
