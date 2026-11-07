#!/usr/bin/env bash
# Mock-PATH unit test for the docker-push plugin entrypoint. Stubs `docker` so
# no real daemon/registry is touched, and locks the contract:
#   - PLUGIN_SOURCE and PLUGIN_TARGET are both required (exit 2, nothing runs),
#   - every requested tag produces a `docker tag` + `docker push`,
#   - PLUGIN_PULL gates the `docker pull` (true → pulls, default → does not),
#   - login happens only when a username is set, and the password never lands
#     on argv or in the log.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub docker: append the FULL argv of every call to the log; everything
# succeeds. `login` reads the password on stdin (never argv), so drain it.
cat >"$TMP/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$TMP_LOG"
[ "${1:-}" = "login" ] && cat >/dev/null
exit 0
EOF
chmod +x "$TMP/docker"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=1; }

run() { # run <logfile> <env...>
  local lf="$1"; shift; : >"$lf"
  set +e
  OUT="$(env TMP_LOG="$lf" PATH="$TMP:$PATH" "$@" bash "$HERE/entrypoint.sh" 2>&1)"; RC=$?
  set -e
}

echo "== 1. PLUGIN_SOURCE required → exit 2, nothing runs =="
run "$TMP/l1" PLUGIN_TARGET=reg/app
[ "$RC" = 2 ] && pass "exit 2 without SOURCE" || fail "expected exit 2, got $RC — $OUT"
[ -s "$TMP/l1" ] && fail "no docker call should happen without SOURCE" || pass "no docker call without SOURCE"

echo "== 2. PLUGIN_TARGET required → exit 2, nothing runs =="
run "$TMP/l2" PLUGIN_SOURCE=app:latest
[ "$RC" = 2 ] && pass "exit 2 without TARGET" || fail "expected exit 2, got $RC — $OUT"
[ -s "$TMP/l2" ] && fail "no docker call should happen without TARGET" || pass "no docker call without TARGET"

echo "== 3. two tags → tag+push each; no pull by default; no login w/o user =="
run "$TMP/l3" PLUGIN_SOURCE=app:latest PLUGIN_TARGET=reg/app PLUGIN_TAGS="r1 r2"
[ "$RC" = 0 ] && pass "exit 0" || fail "expected exit 0, got $RC — $OUT"
[ "$(grep -c '^tag ' "$TMP/l3")" = 2 ] && pass "two docker tag calls" || fail "expected 2 tag calls; log: $(tr '\n' '|' <"$TMP/l3")"
[ "$(grep -c '^push reg/app:' "$TMP/l3")" = 2 ] && pass "two docker push calls" || fail "expected 2 push calls"
grep -q '^tag app:latest reg/app:r1$' "$TMP/l3" && pass "tags source → target:r1" || fail "missing 'tag app:latest reg/app:r1'"
grep -q '^pull ' "$TMP/l3" && fail "must NOT pull when PLUGIN_PULL unset" || pass "no pull by default"
grep -q '^login ' "$TMP/l3" && fail "must NOT login without a username" || pass "no login without username"

echo "== 4. PLUGIN_PULL=true → pulls the source first =="
run "$TMP/l4" PLUGIN_SOURCE=ghcr.io/org/app:x PLUGIN_TARGET=reg/app PLUGIN_PULL=true
[ "$RC" = 0 ] && pass "exit 0" || fail "expected exit 0, got $RC — $OUT"
grep -q '^pull ghcr.io/org/app:x$' "$TMP/l4" && pass "pulled the source" || fail "PLUGIN_PULL=true did not pull; log: $(tr '\n' '|' <"$TMP/l4")"

echo "== 5. login only with a username; password never on argv/log =="
PW="sup3r-s3cret-$$"
run "$TMP/l5" PLUGIN_SOURCE=app:latest PLUGIN_TARGET=reg.example/app \
  PLUGIN_REGISTRY=reg.example PLUGIN_USERNAME=robot PLUGIN_PASSWORD="$PW"
[ "$RC" = 0 ] && pass "exit 0" || fail "expected exit 0, got $RC — $OUT"
grep -q '^login reg.example ' "$TMP/l5" && pass "logged into the registry" || fail "expected a login call; log: $(tr '\n' '|' <"$TMP/l5")"
grep -q "$PW" "$TMP/l5" && fail "password leaked onto argv" || pass "password not on argv"
grep -q "$PW" <<<"$OUT" && fail "password leaked into the log" || pass "password not in the log"

if [ "$fails" = 0 ]; then echo "ALL PASS"; else echo "SOME FAILED"; exit 1; fi
