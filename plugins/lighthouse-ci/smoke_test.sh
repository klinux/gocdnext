#!/usr/bin/env bash
# Real smoke for the lighthouse-ci plugin. Runs the PINNED IMAGE end to end
# against a LOCAL static page (LHCI serves it itself via staticDistDir — no
# network, no external URL), using the plugin's documented auto-discovery path
# (a `lighthouserc.json` at the working-dir root, which collect + assert + upload
# all pick up). Proves:
#   - `lhci collect` drives the bundled Chromium headless and writes a report
#     (also proving the forced node-tar >= 7.5.19 override didn't break LHCI),
#   - `lhci assert` actually gates: passes on a satisfiable budget, FAILS the job
#     on a breached one.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker
# (Chromium runs headless with --no-sandbox as root in the container). Trap
# cleanup; no network.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-lighthouse-ci:smoke-$$"
W="$(mktemp -d)"
cleanup() {
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
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

# Build from scratch: this image refreshes Chromium via apt at build time, and
# the smoke must validate exactly what the publish build ships (which also uses
# no_cache). Mirrors plugins.yml for this plugin.
echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

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

echo "PASS: lighthouse-ci smoke"
