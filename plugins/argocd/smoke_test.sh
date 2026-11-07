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

arg() { docker run --rm --entrypoint argocd "$IMG" "$@" 2>&1; }

echo "==> argocd client is v3.6.1"
OUT="$(arg version --client)" || die "argocd version --client failed" "$OUT"
grep -q "v3.6.1" <<<"$OUT" || die "argocd is not v3.6.1" "$OUT"

echo "==> subcommands the plugin drives exist"
for sub in "app sync" "app wait" "app rollback"; do
  # shellcheck disable=SC2086
  OUT="$(arg $sub --help)" || die "'argocd $sub' missing in v3.6.1" "$OUT"
done

echo "==> flags the plugin passes exist"
SYNC_HELP="$(arg app sync --help)"
for f in --insecure --server-crt --grpc-web; do
  grep -q -- "$f" <<<"$SYNC_HELP" || die "flag $f missing from 'argocd app sync'" "$SYNC_HELP"
done
grep -q -- "--plugin-env" <<<"$(arg app set --help)" || die "--plugin-env missing from 'argocd app set'"

echo "PASS: argocd smoke"
