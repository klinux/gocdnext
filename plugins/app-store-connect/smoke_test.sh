#!/usr/bin/env bash
# Build the real image, prove JWT/crypto/HTTP dependencies import, and execute
# the complete Build Upload + TestFlight transaction against the fake service.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-app-store-connect:smoke-$$"
SMOKE_OK=0

cleanup() {
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  docker image inspect "$IMG" >/dev/null 2>&1 && {
    echo "WARN: smoke image not removed" >&2
    [ "$SMOKE_OK" = 1 ] && exit 1
  }
  return 0
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -60; }; exit 1; }

echo "==> build plugin image (--no-cache, official context)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> JWT, crypto, requests and publisher import in-image"
docker run --rm --network none --entrypoint python3 -e PYTHONPATH=/opt/app-store-connect "$IMG" \
  -c "import jwt, cryptography, requests, publish; print('imports ok')" >/dev/null \
  || die "runtime dependencies failed to import"

echo "==> Build Upload + TestFlight transaction passes against fake service"
set +e
OUT="$(timeout --kill-after=15s 120s docker run --rm --network none \
  -e PYTHONPATH=/opt/app-store-connect \
  -v "$HERE/publish_test.py:/t/publish_test.py:ro" \
  --entrypoint python3 "$IMG" /t/publish_test.py 2>&1)"
RC=$?
set -e
case "$RC" in
  124|137) die "publish_test hung (watchdog RC=$RC)" "$OUT" ;;
esac
[ "$RC" -eq 0 ] || die "publish_test.py failed (rc=$RC)" "$OUT"
grep -q 'PASS: app-store-connect publish unit' <<<"$OUT" || die "publish_test did not report PASS" "$OUT"

SMOKE_OK=1
echo "PASS: app-store-connect smoke"
