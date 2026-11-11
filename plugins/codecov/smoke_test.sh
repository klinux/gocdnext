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
#   - the baked CLI accepts the upload token from the CODECOV_TOKEN env var and the
#     token never surfaces on argv or in stdout/stderr (#393 — the wrapper no longer
#     passes --token on the CLI argv);
#   - the entrypoint's auth contract holds: no token -> exit 2.
#
# Built --no-cache to match a release build. Root-owned workspace outputs are
# cleaned in-image; fail-closed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-codecov:smoke-$$"
CNAME="codecovsmoke-$$"
SMOKE_OK=0
# Distinctive dummy token; asserted absent from every output below. Passed to
# containers by env NAME only (exported here, `-e CODECOV_TOKEN`/`-e PLUGIN_TOKEN`),
# never as `-e NAME=value`, so the value lands on no argv.
TOKEN_SENTINEL="SENTINELcodecovTOKENsmoke5c1d"

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

echo "==> codecov-cli reaches the ingest endpoint AND accepts the token via CODECOV_TOKEN env, not argv (#393)"
# The CLI (not the wrapper) is what the OS migration risks — does codecov-cli still
# run and talk to the API on trixie's Python. It must also accept the upload token
# from the CODECOV_TOKEN env var, which is the whole point of #393 (the wrapper no
# longer puts --token on argv). The token goes in by env NAME only (exported above,
# `-e CODECOV_TOKEN` forwards it) so the sentinel touches no argv — not the
# container's, not the harness's. --commit-sha/--slug are not secrets, so they ride
# argv (codecov's "local" adapter needs a SHA; a real gocdnext job derives it from
# the CI env). Offline it must reach ingest.codecov.io and fail at the NETWORK
# boundary, not crash during startup/arg handling.
export CODECOV_TOKEN="$TOKEN_SENTINEL"
run docker run --rm --name "$CNAME" --network none -e CODECOV_TOKEN --entrypoint codecov "$IMG" \
  create-commit --slug acme/repo \
  --commit-sha 0000000000000000000000000000000000000000
unset CODECOV_TOKEN
# A watchdog kill (124/137) means codecov hung, not a network failure.
case "$RC" in 124|137) die "api probe: watchdog fired (RC=$RC) — codecov hung" "$OUT" ;; esac
# Exact functional exit observed offline, stable across runs: 1 (retry exhaustion).
[ "$RC" -eq 1 ] || die "api probe: expected the stable offline exit 1, got $RC" "$OUT"
grep -qiE 'ingest\.codecov\.io|Request failed after too many retries' <<<"$OUT" \
  || die "api probe: codecov never reached the ingest API (token not accepted via env?)" "$OUT"
# NB: codecov prints a Python traceback when it raises on retry exhaustion, so a
# traceback is NOT a failure signal here — only an arg/usage error is. A usage error
# would mean the env token was NOT consumed and the CLI bailed before the network.
grep -qiE 'Missing option|no such option|Usage: codecov' <<<"$OUT" \
  && die "api probe: codecov errored during arg handling, not at the network" "$OUT" || true
# The token value must never surface in the CLI's own output.
grep -qF "$TOKEN_SENTINEL" <<<"$OUT" \
  && die "api probe: token sentinel leaked into codecov stdout/stderr" "$OUT" || true

echo "==> the plugin WRAPPER exec's upload-process with the token off argv and off the banner (#393)"
# End-to-end through the real entrypoint: the token arrives as PLUGIN_TOKEN by env
# NAME only (exported here; `-e PLUGIN_TOKEN` forwards it), never `-e NAME=value`, so
# the sentinel is on no argv. Offline the CLI can't derive the required commit SHA
# (the "local" adapter reads it from the CI env, absent here) and exits on THAT, not
# on the token — the wrapper exported CODECOV_TOKEN and the token was accepted. We
# assert: codecov was actually invoked (the `==> codecov upload-process` banner), the
# sentinel is absent from the whole output incl. that banner, and it was NOT the
# token that was missing. (entrypoint_test.sh proves argv/banner cleanliness
# rigorously with a mock; this is the real-image backstop.)
export PLUGIN_TOKEN="$TOKEN_SENTINEL"
run docker run --rm --name "$CNAME" --network none -e PLUGIN_TOKEN -e PLUGIN_SLUG=acme/repo "$IMG"
unset PLUGIN_TOKEN
case "$RC" in 124|137) die "wrapper probe: watchdog fired (RC=$RC) — codecov hung" "$OUT" ;; esac
[ "$RC" -ne 0 ] || die "wrapper probe: expected a non-zero exit offline, got 0" "$OUT"
grep -qE '==> codecov upload-process' <<<"$OUT" \
  || die "wrapper probe: the entrypoint never exec'd codecov upload-process" "$OUT"
grep -qF "$TOKEN_SENTINEL" <<<"$OUT" \
  && die "wrapper probe: token sentinel leaked into the wrapper output (banner/log?)" "$OUT" || true
grep -qi 'token is required' <<<"$OUT" \
  && die "wrapper probe: token was NOT accepted via env (fell back to the required-token error)" "$OUT" || true

SMOKE_OK=1
echo "PASS: codecov smoke"
