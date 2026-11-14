#!/usr/bin/env bash
# Real smoke for the firebase-app-distribution plugin's IMAGE. Builds the pinned
# image and proves the Firebase CLI is installed and the appdistribution
# subcommand the entrypoint invokes actually exists (a typo'd/renamed subcommand
# would fail HERE, before publish). A real distribute needs a live Firebase
# project + service account, so it can't run in CI — the entrypoint's validation
# and secret handling are covered hermetically by entrypoint_test.sh.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-firebase-app-distribution:smoke-$$"
SMOKE_OK=0

cleanup() {
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  docker image inspect "$IMG" >/dev/null 2>&1 && { echo "WARN: smoke image not removed" >&2; [ "$SMOKE_OK" = 1 ] && exit 1; }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> firebase CLI is installed and reports a version"
VER="$(docker run --rm --network none --entrypoint firebase "$IMG" --version 2>&1)" \
  || die "firebase --version failed" "$VER"
grep -qE '^[0-9]+\.[0-9]+\.[0-9]+' <<<"$VER" || die "unexpected firebase version output" "$VER"
echo "    firebase ${VER}"

echo "==> the appdistribution:distribute subcommand exists (the one the entrypoint calls)"
set +e
OUT="$(timeout --kill-after=10s 60s docker run --rm --network none --entrypoint firebase "$IMG" \
  appdistribution:distribute --help 2>&1)"
RC=$?
set -e
case "$RC" in
  124|137) die "firebase --help hung (watchdog RC=$RC)" "$OUT" ;;
esac
[ "$RC" -eq 0 ] || die "appdistribution:distribute subcommand missing/renamed (rc=$RC)" "$OUT"
grep -qi 'release-notes-file' <<<"$OUT" || die "--release-notes-file flag (used by the entrypoint) not found in help" "$OUT"
grep -qi -- '--app' <<<"$OUT" || die "--app flag (used by the entrypoint) not found in help" "$OUT"

SMOKE_OK=1
echo "PASS: firebase-app-distribution smoke"
