#!/usr/bin/env bash
# Real smoke for the lighthouse-ci plugin. Runs the PINNED IMAGE end to end
# against a LOCAL static page (LHCI serves it itself via staticDistDir — no
# network, no external URL), using the plugin's documented auto-discovery path
# (a `lighthouserc.json` at the working-dir root, which collect + assert + upload
# all pick up). Proves:
#   - `lhci collect` drives the bundled Chromium headless and writes a report,
#   - `lhci assert` actually gates: passes on a satisfiable budget, FAILS the job
#     on a breached one.
#   - `upload-target=lhci` hands the build token to the SHIPPED lhci 0.16.0 via
#     the LHCI_TOKEN env var (#339): the token never appears on argv or in the
#     job output, and LHCI reaches the "connect to the server" stage — proving
#     the env channel is honoured by the real binary, not just the unit mock.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker
# (Chromium runs headless with --no-sandbox as root in the container). Trap
# cleanup; no network.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-lighthouse-ci:smoke-$$"
W="$(mktemp -d)"
CLHCI="gocdnext-lhci-smoke-$$"   # named so cleanup can fail-closed remove it
# Distinctive token value: if a copy of it survives anywhere in the job output
# or on argv, it is unmistakably the leak we are testing for.
TOKEN_SENTINEL="SENTINELlhciSMOKEtoken7a1c"
SMOKE_OK=0
cleanup() {
  # Fail-closed: kill the named lhci-upload container FIRST, in case a watchdog
  # SIGKILL left it running (--rm then can't fire). Only then touch the image
  # and workspace.
  docker rm -f "$CLHCI" >/dev/null 2>&1 || true
  # LHCI runs as root in the container and leaves root-owned files (incl.
  # dotdirs like .lighthouseci) under $W. Remove them from INSIDE the image —
  # with --entrypoint sh so the command actually runs, and before deleting the
  # image — then drop the dir.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$W:/w" "$IMG" \
      -c 'rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  # Don't silently swallow a failed cleanup: if the workspace survived, say so,
  # and fail the run when the smoke itself had otherwise passed (a leaked
  # root-owned dir is a real defect to surface, not to hide).
  if [ -d "$W" ]; then
    echo "WARN: cleanup could not fully remove ${W} (root-owned leftovers?)" >&2
    [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left files behind" >&2; exit 1; }
  fi
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Build from scratch: this image refreshes Chromium via apt at build time, and
# the smoke must validate exactly what the publish build ships (which also uses
# no_cache). Mirrors plugins.yml for this plugin.
echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

# 0. base is trixie and the OS packages this migration clears carry the fix.
#    bookworm had no fix for libsqlite3-0 (CVE-2025-7458), glib
#    (CVE-2026-58016) or zlib1g (CVE-2023-45853); trixie ships all three fixed.
#    (libxml2 CVE-2026-6653 is "no-dsa" on trixie too, so it is NOT guarded here
#    — it stays signed off in plugins/.trivy-baseline.yaml.) Compare with dpkg,
#    NOT a regex: Debian versions don't sort lexically. Floors = FIRST trixie
#    fixed version per the Debian security tracker: zlib1g >= 1:1.3,
#    libsqlite3-0 >= 3.46, libglib2.0-0t64 >= 2.84.4-3~deb13u4 (CVE-2026-58016
#    was first fixed in deb13u4; the currently-installed build is deb13u5, but
#    the floor must be the first fix so a valid u4 image doesn't false-red).
osv="$(docker run --rm --network none --entrypoint sh "$IMG" -c '
  printf "debian %s\n" "$(cat /etc/debian_version)"
  z="$(dpkg-query -W -f="\${Version}" zlib1g)"
  s="$(dpkg-query -W -f="\${Version}" libsqlite3-0)"
  g="$(dpkg-query -W -f="\${Version}" libglib2.0-0t64)"
  printf "zlib1g %s\n" "$z"; printf "libsqlite3-0 %s\n" "$s"; printf "libglib2.0-0t64 %s\n" "$g"
  dpkg --compare-versions "$z" ge "1:1.3"              && echo ZLIB_FIXED   || echo ZLIB_UNFIXED
  dpkg --compare-versions "$s" ge "3.46"               && echo SQLITE_FIXED || echo SQLITE_UNFIXED
  dpkg --compare-versions "$g" ge "2.84.4-3~deb13u4"   && echo GLIB_FIXED   || echo GLIB_UNFIXED
')" || die "OS probe failed" "$osv"
grep -qE '^debian 13\.' <<<"$osv" || die "base is not Debian trixie (13.x)" "$osv"
grep -qx 'ZLIB_FIXED'   <<<"$osv" || die "zlib1g does not carry the CVE-2023-45853 fix (>= 1:1.3)" "$osv"
grep -qx 'SQLITE_FIXED' <<<"$osv" || die "libsqlite3-0 does not carry the CVE-2025-7458 fix (>= 3.46)" "$osv"
grep -qx 'GLIB_FIXED'   <<<"$osv" || die "libglib2.0-0t64 does not carry the CVE-2026-58016 fix (>= 2.84.4-3~deb13u4)" "$osv"

CHROME_FLAGS="--no-sandbox --headless=new --disable-dev-shm-usage --disable-gpu"

# make_case <dir> <assertions-json>: a workspace with a static page + an
# auto-discovered lighthouserc.json carrying the given assertions.
make_case() {
  local d="$W/$1"; shift
  mkdir -p "$d/public"
  cat >"$d/public/index.html" <<'HTML'
<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>smoke</title></head>
<body><h1>gocdnext lighthouse-ci smoke</h1><p>hello</p></body></html>
HTML
  cat >"$d/lighthouserc.json" <<JSON
{
  "ci": {
    "collect": { "staticDistDir": "./public", "numberOfRuns": 1,
                 "settings": { "chromeFlags": "${CHROME_FLAGS}" } },
    "assert": { "assertions": $1 }
  }
}
JSON
}

run() { # run <dir> — auto-discovery (no PLUGIN_CONFIG/PLUGIN_URLS); sets RC + OUT
  set +e
  OUT="$(docker run --rm -v "$W/$1:/workspace" -w /workspace \
    -e PLUGIN_UPLOAD_TARGET=filesystem -e PLUGIN_ASSERTIONS=on \
    "$IMG" 2>&1)"; RC=$?
  set -e
}

# 1. satisfiable assertions -> collect + assert + upload all succeed.
make_case pass '{ "viewport": ["error", {}], "document-title": ["error", {}] }'
run pass
[ "$RC" -eq 0 ] || die "collect+assert+upload should succeed on a satisfiable budget (rc=$RC)" "$OUT"
grep -q "lhci collect" <<<"$OUT" || die "collect stage did not run" "$OUT"
grep -q "lhci assert" <<<"$OUT" || die "assert stage did not run" "$OUT"
ls "$W"/pass/.lighthouseci/lhr-*.json >/dev/null 2>&1 \
  || die "collect produced no Lighthouse report" "$OUT"

# 2. a breached assertion must FAIL the job (proves the gate actually gates).
#    An FCP ceiling of 0ms is impossible to meet, so assert must exit non-zero.
make_case fail '{ "first-contentful-paint": ["error", { "maxNumericValue": 0 }] }'
run fail
[ "$RC" -ne 0 ] || die "a breached assertion must fail the job, but it exited 0" "$OUT"
# Require the SPECIFIC assertion to appear in LHCI's assert output — a bare
# "assert" match would also catch the wrapper's own "==> lhci assert" log, so a
# mere crash of `lhci assert` would pass. The assertion name only appears when
# LHCI actually evaluated (and failed) it.
grep -q "first-contentful-paint" <<<"$OUT" \
  || die "expected lhci assert to report the breached first-contentful-paint assertion" "$OUT"

# 3. upload-target=lhci: the build token reaches the SHIPPED lhci binary via the
#    LHCI_TOKEN env var (#339) — not argv, not the job log. We point the upload
#    at an unreachable server (127.0.0.1:9, the discard port) under --network
#    none so the only thing that can happen is an immediate connection refusal.
#    What we assert:
#      * lhci gets PAST the token guard (NO "Must provide token") and fails on
#        the CONNECTION — only possible if it read the token from LHCI_TOKEN,
#        since nothing is passed on argv;
#      * the token sentinel appears NOWHERE in stdout/stderr (the `==> lhci
#        upload ...` banner and LHCI's own logging are both clean);
#      * the exit is the exact functional failure (1), not a watchdog kill.
#    The sentinel is handed to `docker run` by env NAME only (-e PLUGIN_UPLOAD_
#    TOKEN with the value exported just for that command), so it never lands on
#    this script's own argv either.
make_case lhci '{}'
set +e
OUT="$(PLUGIN_UPLOAD_TOKEN="$TOKEN_SENTINEL" timeout --kill-after=10s 60s \
  docker run --rm --network none --name "$CLHCI" \
    -v "$W/lhci:/workspace" -w /workspace \
    -e PLUGIN_UPLOAD_TARGET=lhci \
    -e PLUGIN_UPLOAD_SERVER_BASE_URL=http://127.0.0.1:9 \
    -e PLUGIN_ASSERTIONS=off \
    -e PLUGIN_UPLOAD_TOKEN \
    "$IMG" 2>&1)"; RC=$?
set -e
case "$RC" in
  124) die "lhci upload hung (watchdog timeout 60s fired) — expected a fast connection refusal" "$OUT" ;;
  137) die "lhci upload had to be SIGKILLed (watchdog --kill-after) — it hung" "$OUT" ;;
  1)   ;; # the exact functional exit observed: upload failed on the connection
  0)   die "lhci upload exited 0 against an unreachable server — unexpected" "$OUT" ;;
  *)   die "lhci upload exited with unexpected code $RC (expected 1)" "$OUT" ;;
esac
# Got past the token requirement (so LHCI_TOKEN was honoured)...
grep -qi "Must provide token" <<<"$OUT" \
  && die "lhci still demands a token — LHCI_TOKEN was not honoured by the shipped binary" "$OUT"
# ...and failed on the connection to the unreachable server.
grep -qi "ECONNREFUSED" <<<"$OUT" \
  || die "expected a connection-refused error from the unreachable lhci server" "$OUT"
# The token never surfaced in any output.
grep -qF "$TOKEN_SENTINEL" <<<"$OUT" \
  && die "build-token sentinel leaked into the lhci upload output" "$OUT"
# The upload banner ran but carried no token.
grep -q "lhci upload" <<<"$OUT" || die "upload stage did not run for upload-target=lhci" "$OUT"
docker rm -f "$CLHCI" >/dev/null 2>&1 || true

SMOKE_OK=1
echo "PASS: lighthouse-ci smoke"
