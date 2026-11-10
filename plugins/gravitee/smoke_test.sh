#!/usr/bin/env bash
# Real smoke for the gravitee plugin's IMAGE toolchain. A full apply/deploy needs
# a live Gravitee Management API, so this proves the parts that run WITHOUT a
# server and that the Python 3.9 -> 3.12 trixie port (#341) didn't break the
# toolchain:
#   - the base is trixie with the migrated OS packages at their FIXED versions,
#   - the graviteeio-cli fork (`gio`) still runs on the new python:3.12 base
#     (generate + lint on 3.12, where the fork was built for 3.7–3.9),
#   - the bumped `yq` runs and is v4.54.2 (deep-merge tool the entrypoint needs),
#   - the bundled `gravitee-validate` security linter runs on the image's Python:
#     it PASSES a locked-down definition and BLOCKS an open one.
# (The wrapper's merge / env-substitution / token-off-argv behaviour is covered
# by entrypoint_test.sh against the real yq/jq.)
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-gravitee:smoke-$$"
W="$(mktemp -d)"
NET="gravnet-$$"       # user-defined docker network for the mock Management API
MOCK="gravmock-$$"     # mock-server container name
CONS="gravcons-$$"     # gio CONSUMER container name (named so a watchdog kill
                       # can't orphan it on the network; see the async section)
cleanup() {
  # Order: consumer first (it's attached to the network), then the mock, then
  # the network (now with nothing attached), then the image and the workdir.
  docker rm -f "$CONS" >/dev/null 2>&1 || true
  docker rm -f "$MOCK" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

# base is trixie and the OS packages the port clears carry the fix. bookworm had
# no fix for libsqlite3-0 (CVE-2025-7458) or zlib1g (CVE-2023-45853); trixie
# ships both fixed. Compare with dpkg, NOT a regex (Debian versions don't sort
# lexically). Floors = first trixie fixed version: zlib1g >= 1:1.3,
# libsqlite3-0 >= 3.46.
echo "==> base is trixie and the migrated OS packages carry the fix"
osv="$(docker run --rm --network none --entrypoint sh "$IMG" -c '
  printf "debian %s\n" "$(cat /etc/debian_version)"
  z="$(dpkg-query -W -f="\${Version}" zlib1g)"
  s="$(dpkg-query -W -f="\${Version}" libsqlite3-0)"
  printf "zlib1g %s\n" "$z"; printf "libsqlite3-0 %s\n" "$s"
  dpkg --compare-versions "$z" ge "1:1.3" && echo ZLIB_FIXED   || echo ZLIB_UNFIXED
  dpkg --compare-versions "$s" ge "3.46"  && echo SQLITE_FIXED || echo SQLITE_UNFIXED
')" || die "OS probe failed" "$osv"
grep -qE '^debian 13\.' <<<"$osv" || die "base is not Debian trixie (13.x)" "$osv"
grep -qx 'ZLIB_FIXED'   <<<"$osv" || die "zlib1g does not carry the CVE-2023-45853 fix (>= 1:1.3)" "$osv"
grep -qx 'SQLITE_FIXED' <<<"$osv" || die "libsqlite3-0 does not carry the CVE-2025-7458 fix (>= 3.46)" "$osv"

echo "==> gio runs on the new python:3.12 base"
OUT="$(docker run --rm --entrypoint gio "$IMG" --version 2>&1)" || die "gio --version failed" "$OUT"
grep -q "0.4.1" <<<"$OUT" || die "unexpected gio version" "$OUT"
# the dead asyncio backport must NOT shadow the stdlib on 3.12 (if it did, gio's
# async paths would silently run a 2015 asyncio). Prove stdlib wins.
AF="$(docker run --rm --entrypoint python3 "$IMG" -c 'import asyncio; print(asyncio.__file__)' 2>&1)" \
  || die "python3 import asyncio failed" "$AF"
grep -q '/usr/local/lib/python3.12/asyncio/' <<<"$AF" \
  || die "stdlib asyncio is shadowed by the backport (resolved to: $AF)"

# gio's ASYNC path runs on every real invocation: the entrypoint resolves the
# API id by name with `gio apim apis list` (entrypoint.sh), which drives
# ls.py -> api_async.py (asyncio.get_event_loop + run_until_complete + a
# ThreadPoolExecutor that fetches each API's /state). get_event_loop is
# DEPRECATED but not removed in 3.12, so this is exactly what the port must
# prove. Stand up a mock Management API (served by the image's OWN python3) on a
# private docker network and run the real command against it: it exercises the
# event loop + executor AND the refreshed requests/urllib3 over a live socket.
echo "==> gio async list path works on 3.12 (real event loop + executor + HTTP)"
cat >"$W/mock.py" <<'PY'
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer
API_ID, API_NAME = "smoke-api-id", "smoke-api"
class H(BaseHTTPRequestHandler):
    def _send(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-type", "application/json")
        self.send_header("Content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        p = self.path.split("?", 1)[0]
        if p.endswith("/state"):                       # per-API state (executor)
            self._send({"is_synchronized": True})
        elif p.rstrip("/").endswith("/apis"):          # the listing endpoint
            self._send([{"id": API_ID, "name": API_NAME, "tags": [],
                         "state": "started", "workflow_state": None}])
        else:
            self._send([])
    def log_message(self, *a):  # quiet
        pass
if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 8080), H).serve_forever()
PY
docker network create "$NET" >/dev/null || die "could not create docker network"
docker run -d --name "$MOCK" --network "$NET" -v "$W:/w:ro" \
  --entrypoint python3 "$IMG" /w/mock.py >/dev/null || die "could not start mock server"
# Readiness: poll the listing endpoint (bounded) before invoking gio.
ready=0
for _ in $(seq 1 15); do
  if docker run --rm --network "$NET" --entrypoint curl "$IMG" -fsS -m 2 \
       "http://$MOCK:8080/management/organizations/DEFAULT/environments/DEFAULT/apis/" \
       >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || die "mock Management API did not become ready" "$(docker logs "$MOCK" 2>&1 | tail -20)"

# Named + pre-removed consumer: if the watchdog kills the docker CLI, --rm may not
# fire, so a leftover named container could stay attached to $NET (blocking its
# removal). Removing "$CONS" before each run (and first in cleanup) is fail-closed.
docker rm -f "$CONS" >/dev/null 2>&1 || true
set +e
OUT="$(timeout --kill-after=10s 120s docker run --rm --name "$CONS" --network "$NET" \
  -e GIO_APIM_URL="http://$MOCK:8080" -e GIO_APIM_TOKEN=smoke-dummy \
  -e GIO_APIM_ORG=DEFAULT -e GIO_APIM_ENV=DEFAULT \
  --entrypoint gio "$IMG" apim apis list -q "[?name=='smoke-api'].id" -o json 2>&1)"
RC=$?
set -e
case "$RC" in 124|137) die "async list timed out (watchdog RC=$RC) — event loop hung on 3.12" "$OUT" ;; esac
[ "$RC" -eq 0 ] || die "gio apim apis list (async path) exited $RC on 3.12" "$OUT"
grep -q 'smoke-api-id' <<<"$OUT" || die "async list did not return the expected API id" "$OUT"
# Prove the executor-driven /state fetch populated is_synchronized (not just the
# bare listing): a second projection must surface the value the state endpoint returned.
docker rm -f "$CONS" >/dev/null 2>&1 || true
set +e
OUT2="$(timeout --kill-after=10s 120s docker run --rm --name "$CONS" --network "$NET" \
  -e GIO_APIM_URL="http://$MOCK:8080" -e GIO_APIM_TOKEN=smoke-dummy \
  -e GIO_APIM_ORG=DEFAULT -e GIO_APIM_ENV=DEFAULT \
  --entrypoint gio "$IMG" apim apis list -q "[?name=='smoke-api'].is_synchronized" -o json 2>&1)"
RC2=$?
set -e
case "$RC2" in 124|137) die "async state projection timed out (watchdog RC=$RC2) — event loop hung" "$OUT2" ;; esac
[ "$RC2" -eq 0 ] || die "gio async state projection exited $RC2" "$OUT2"
grep -q 'true' <<<"$OUT2" || die "executor /state fetch did not populate is_synchronized" "$OUT2"
docker rm -f "$CONS" >/dev/null 2>&1 || true
docker rm -f "$MOCK" >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true

echo "==> yq is the bumped v4.54.2"
OUT="$(docker run --rm --entrypoint yq "$IMG" --version 2>&1)" || die "yq --version failed" "$OUT"
grep -q "4.54.2" <<<"$OUT" || die "yq is not v4.54.2" "$OUT"

# Exercise the EXACT merge operators the entrypoint depends on (the yq bump is the
# risk): deep merge with array CONCAT (`*+` ireduce, the `merge` mode) vs
# merge-assign that REPLACES arrays (`*= load()`, the `overwrite` mode).
printf 'a: 1\nlist: [x]\nmap: {k1: v1}\n' >"$W/defaults.yml"
printf 'b: 2\nlist: [y]\nmap: {k2: v2}\n' >"$W/values.yml"
yqrun() { docker run --rm -v "$W:/w" -w /w --entrypoint yq "$IMG" "$@" 2>&1; }

echo "==> merge mode (*+): arrays concat, maps deep-merge"
M="$(yqrun -o=json -I=0 eval-all '. as $i ireduce ({}; . *+ $i)' defaults.yml values.yml)" \
  || die "merge yq failed" "$M"
grep -q '"list":\["x","y"\]' <<<"$M" || die "merge did not concat arrays (got: $M)"
grep -q '"k1":"v1"' <<<"$M" && grep -q '"k2":"v2"' <<<"$M" || die "merge did not deep-merge maps (got: $M)"

echo "==> overwrite mode (*= load): arrays replaced"
O="$(yqrun -o=json -I=0 '. *= load("values.yml")' defaults.yml)" || die "overwrite yq failed" "$O"
grep -q '"list":\["y"\]' <<<"$O" || die "overwrite did not replace the array (got: $O)"

echo "==> gio generate + real offline lint (clean definition -> errors(0))"
L="$(docker run --rm --entrypoint sh "$IMG" -c '
  set -e
  d=$(mktemp -d)
  gio apim apis definition generate --def-path "$d" >/dev/null 2>&1
  # the scaffold ships a placeholder visibility the schema rejects; set the
  # valid enum so a clean definition really lints clean.
  sed -i "s/visibility: private/visibility: PRIVATE/" "$d/templates/api_config.yml.j2"
  gio apim apis definition lint --def-path "$d" 2>&1 | tail -1' 2>&1)" || die "gio lint invocation failed" "$L"
grep -q "errors(0)" <<<"$L" || die "gio offline lint did not report a clean definition (got: $L)"

# gravitee-validate: a locked-down definition PASSES, an open one BLOCKS.
cat >"$W/clean.json" <<'JSON'
{ "paths": { "/orders": [ { "methods": ["GET","POST"], "jwt": {} } ] } }
JSON
cat >"$W/open.json" <<'JSON'
{ "paths": { "/orders": [ { "transform-headers": {} } ] } }
JSON

echo "==> gravitee-validate PASSES a locked-down definition"
set +e
OUT="$(docker run --rm -v "$W:/w" --entrypoint gravitee-validate "$IMG" \
  --methods block --auth block /w/clean.json 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || die "validator should pass a methods+auth-locked definition (rc=$RC)" "$OUT"

echo "==> gravitee-validate BLOCKS an open (no methods, no auth) definition"
set +e
OUT="$(docker run --rm -v "$W:/w" --entrypoint gravitee-validate "$IMG" \
  --methods block --auth block /w/open.json 2>&1)"; RC=$?
set -e
[ "$RC" -eq 2 ] || die "validator should BLOCK (exit 2) an open definition (rc=$RC)" "$OUT"
grep -q "no explicit methods" <<<"$OUT" || die "expected the open-methods finding" "$OUT"
grep -q "no auth policy" <<<"$OUT" || die "expected the missing-auth finding" "$OUT"

echo "PASS: gravitee smoke"
