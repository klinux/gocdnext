#!/usr/bin/env bash
# Boot smoke for the web (Next.js) PRODUCTION image (#391).
#
# CI's `pnpm run build` validates the `build` stage, but it NEVER exercises the
# `runner` stage — the standalone node server that actually serves the app in
# production. So a regression in the runtime base (e.g. an EOL/incompatible node,
# a broken standalone copy, a missing docs mount) ships invisibly. This builds
# web/Dockerfile end to end and boots the final image, asserting:
#   - the runtime is node 24 (the migration off EOL node 20);
#   - the standalone server comes up and serves a REAL route: GET /docs → 200
#     with the page's marker. /docs reads bundled markdown from the image, so it
#     needs NO gRPC backend (unlike `/`, which fetches the API) — hermetic.
#
# Build context is the repo ROOT (the Dockerfile COPYs web/ AND docs/). The
# script resolves the root itself, so it runs from anywhere (CI calls it from
# web/). Needs docker + network (corepack/pnpm fetch at build).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
IMG="gocdnext-web-smoke:$$"
CID="gocdnext-web-smoke-c-$$"
PORT="${WEB_SMOKE_PORT:-13900}"

cleanup() {
  # Fail-closed: remove the CONTAINER first (a watchdog-killed or still-running
  # one would otherwise hold the published port and the image ref), then the
  # image.
  docker rm -f "$CID" >/dev/null 2>&1 || true
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- container logs ---"; echo "$2" | tail -30; }; exit 1; }

echo "==> build web image (context=repo root, -f web/Dockerfile)"
docker build -f "$ROOT/web/Dockerfile" -t "$IMG" "$ROOT" >/dev/null

echo "==> runtime is node 24"
nv="$(docker run --rm --entrypoint node "$IMG" --version 2>&1)" || die "node probe failed" "$nv"
case "$nv" in
  v24.*) echo "    $nv" ;;
  *)     die "final image runtime is not node 24 (got '$nv') — EOL/incorrect base?" ;;
esac

echo "==> boot the standalone server; GET /docs must return 200"
docker run -d --name "$CID" -p "${PORT}:3000" "$IMG" >/dev/null

# Bounded readiness watchdog: a real ~40s WALL-CLOCK cap (not 40 iterations —
# each probe's curl is capped at 5s + a 1s sleep, which would otherwise run to
# ~240s). The container is removed by the trap on any exit.
code=000
deadline=$(( SECONDS + 40 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/docs" 2>/dev/null || echo 000)"
  [ "$code" = "200" ] && break
  sleep 1
done
[ "$code" = "200" ] || die "GET /docs did not return 200 within 40s (last HTTP ${code})" "$(docker logs "$CID" 2>&1)"

# 200 is necessary but not sufficient — assert the page actually rendered the
# docs index (the standalone server resolved + read the bundled markdown mount),
# not an error page that happened to return 200.
body="$(curl -s --max-time 5 "http://127.0.0.1:${PORT}/docs" 2>/dev/null || true)"
printf '%s' "$body" | grep -qiF 'gocdnext docs' \
  || die "GET /docs returned 200 but the docs-index marker is missing (bad docs mount?)" "$(docker logs "$CID" 2>&1)"

echo "PASS: web docker smoke"
