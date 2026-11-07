#!/usr/bin/env bash
# Real smoke for the argocd plugin's IMAGE. Every argocd action needs a live Argo
# CD API server, so this proves what runs WITHOUT one and that the major CLI bump
# (v2.13.1 -> v3.6.1, the CVE fix) didn't drop the surface the wrapper depends on:
#   - the argocd binary is v3.6.1 and runs,
#   - the subcommands the plugin drives still exist (app sync / wait / rollback),
#   - the flags the plugin passes still exist (--insecure, --server-crt,
#     --grpc-web, and --plugin-env on `app set`).
# (The wrapper's arg handling / token-off-argv is covered by entrypoint_test.sh.)
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-argocd:smoke-$$"
cleanup() { docker image rm -f "$IMG" >/dev/null 2>&1 || true; }
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> argocd client is v3.6.1"
OUT="$(docker run --rm --entrypoint argocd "$IMG" version --client 2>&1)" \
  || die "argocd version --client failed" "$OUT"
grep -q "v3.6.1" <<<"$OUT" || die "argocd is not v3.6.1" "$OUT"

# Run the REAL entrypoint against a dead endpoint (127.0.0.1:1). The command must
# reach the network and fail with a CONNECTION error — NOT a parse/usage error.
# That proves the subcommand + flags the wrapper builds are still accepted by the
# v3 CLI (a removed command/flag would be a parse error, not a connection one).
runep() { # runep <extra -e flags...> : sets RC + OUT
  set +e
  OUT="$(docker run --rm \
    -e PLUGIN_SERVER=127.0.0.1:1 -e PLUGIN_AUTH_TOKEN=smoke -e PLUGIN_INSECURE=true \
    "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}
assert_conn() { # assert_conn <label>
  [ "$RC" -ne 0 ] || die "$1: expected a non-zero connection failure (rc=$RC)" "$OUT"
  grep -qiE 'connection refused|Failed to establish connection' <<<"$OUT" \
    || die "$1: expected a connection error, got something else" "$OUT"
  if grep -qiE 'unknown (flag|command|shorthand)|Error: accepts|requires at least' <<<"$OUT"; then
    die "$1: got a PARSE/usage error — the argocd v3 CLI surface changed" "$OUT"
  fi
}

echo "==> entrypoint 'app sync' reaches the server (connection failure, not parse)"
runep -e PLUGIN_COMMAND="app sync demo"
assert_conn "app sync"

echo "==> entrypoint 'app set' + plugin_env forwards one arg and reaches the server"
runep -e PLUGIN_COMMAND="app set demo" -e PLUGIN_PLUGIN_ENV="HELM_ARGS=--set x -f y"
assert_conn "app set + plugin_env"
grep -q -- "--plugin-env HELM_ARGS=--set x -f y" <<<"$OUT" \
  || die "plugin_env was not forwarded as a single --plugin-env arg" "$OUT"

echo "PASS: argocd smoke"
