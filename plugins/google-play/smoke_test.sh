#!/usr/bin/env bash
# Real smoke for the google-play plugin's IMAGE. Builds the pinned image and:
#   - proves the client libs resolve + publish.py imports in-image;
#   - runs publish_test.py INSIDE the image (real google-api-python-client,
#     fake Play service) so the edit→upload→track→commit transaction logic is
#     exercised end to end without credentials or network to Google.
#
# A real publish can't run in CI (needs a live service account + Play app), so
# the transaction is proven against a fake service; the entrypoint's validation
# and secret handling are covered hermetically by entrypoint_test.sh.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-google-play:smoke-$$"
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

echo "==> client libs resolve and publish.py imports in-image"
docker run --rm --network none --entrypoint python3 -e PYTHONPATH=/opt/gplay "$IMG" \
  -c "import googleapiclient.discovery, google.oauth2.service_account, publish; print('imports ok')" >/dev/null \
  || die "google client libs / publish.py failed to import in-image"

echo "==> publish.py edit transaction passes against a fake Play service (publish_test.py, in-image)"
set +e
OUT="$(timeout --kill-after=15s 120s docker run --rm --network none \
  -e PYTHONPATH=/opt/gplay \
  -v "$HERE/publish_test.py:/t/publish_test.py:ro" \
  --entrypoint python3 "$IMG" /t/publish_test.py 2>&1)"
RC=$?
set -e
case "$RC" in
  124|137) die "publish_test hung (watchdog RC=$RC)" "$OUT" ;;
esac
[ "$RC" -eq 0 ] || die "publish_test.py failed (rc=$RC)" "$OUT"
grep -q 'PASS: google-play publish unit' <<<"$OUT" || die "publish_test did not report PASS" "$OUT"

SMOKE_OK=1
echo "PASS: google-play smoke"
