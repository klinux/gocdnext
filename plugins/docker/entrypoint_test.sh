#!/usr/bin/env bash
# Mock-PATH unit test for the docker plugin entrypoint. Stubs `docker` so no
# real daemon/registry is touched, and asserts the daemon-readiness wait:
# `docker build` only fires once `docker info` answers, and a daemon that
# never comes up fails with a clear message instead of racing into the
# cryptic "Cannot connect to the Docker daemon" one-liner (#build-image DinD).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub docker: log each subcommand; `info` fails until READY_AFTER calls have
# happened (simulating a DinD sidecar still starting), then succeeds; login/
# build/push just succeed.
cat >"$TMP/docker" <<'EOF'
#!/usr/bin/env bash
sub="$1"
printf '%s\n' "$sub" >>"$TMP_LOG"
case "$sub" in
    info)
        n=$(cat "$TMP_INFO_N" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" >"$TMP_INFO_N"
        [ "$n" -ge "${READY_AFTER:-1}" ] && exit 0 || exit 1 ;;
    login|build|push) exit 0 ;;
    *) exit 0 ;;
esac
EOF
chmod +x "$TMP/docker"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=1; }

base_env=(
    PLUGIN_IMAGE=img.cora.tools/public/keycloak
    PLUGIN_TAGS=17.0.1.5-cora
    PLUGIN_CONTEXT=platform/base-images
    PLUGIN_PUSH=true
)

echo "== 1. daemon not ready at first → waits, then builds + pushes =="
log="$TMP/log1"; : >"$log"; : >"$TMP/n1"
out=$(env "${base_env[@]}" READY_AFTER=3 PLUGIN_DAEMON_TIMEOUT=30 \
    TMP_LOG="$log" TMP_INFO_N="$TMP/n1" PATH="$TMP:$PATH" \
    bash "$HERE/entrypoint.sh" 2>&1)
rc=$?
[ "$rc" = 0 ] && pass "exit 0" || fail "exit $rc — $out"
grep -q build "$log" && pass "build ran" || fail "build did not run; log: $(tr '\n' ' ' <"$log")"
grep -q push "$log" && pass "push ran" || fail "push did not run"
# info must have been retried (>=3 calls) BEFORE build
if [ "$(grep -c '^info$' "$log")" -ge 3 ]; then pass "waited on docker info (>=3 polls)"; else fail "did not retry docker info"; fi

echo "== 2. daemon never ready → clear timeout error, NO build =="
log="$TMP/log2"; : >"$log"; : >"$TMP/n2"
out=$(env "${base_env[@]}" READY_AFTER=9999 PLUGIN_DAEMON_TIMEOUT=2 \
    TMP_LOG="$log" TMP_INFO_N="$TMP/n2" PATH="$TMP:$PATH" \
    bash "$HERE/entrypoint.sh" 2>&1)
rc=$?
[ "$rc" = 1 ] && pass "exit 1 on timeout" || fail "expected exit 1, got $rc"
case "$out" in *"Docker daemon not reachable"*) pass "clear timeout message" ;; *) fail "missing clear message; got: $out" ;; esac
grep -q build "$log" && fail "build must NOT run when daemon never came up" || pass "no build on dead daemon"

if [ "$fails" = 0 ]; then echo "ALL PASS"; else echo "SOME FAILED"; exit 1; fi
