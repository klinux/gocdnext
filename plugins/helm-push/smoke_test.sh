#!/usr/bin/env bash
# Real smoke for the helm-push plugin's IMAGE. Pushing a chart needs a live
# registry, so every case anchors on the deterministic NXDOMAIN of
# `example.invalid` (RFC2606): `helm package` runs for real against a minimal
# in-workspace chart, then each backend drives its publish flow until it hits the
# DNS boundary — proving the base bump (alpine/helm:3.16.2 -> 3.22.1) kept the
# plugin's surface:
#   - the helm binary is on the 3.22 line (NOT 4.x);
#   - oci (helm registry login + push), chartmuseum (curl POST) and nexus (curl
#     PUT) each run the REAL entrypoint, package the chart, and fail on the
#     registry/repo DNS lookup — NOT a parse/unknown-flag error;
#   - the registry password never appears in the log/argv.
#
# Convention-named so plugins.yml runs it as a PRE-PUBLISH gate. Needs docker.
# Built --no-cache to match the plugin's no_cache publish build. Trap cleanup.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-helm-push:smoke-$$"
W="$(mktemp -d)"
SMOKE_OK=0
cleanup() {
  # helm package writes to /tmp inside the container, but guard anyway: remove any
  # root-owned files from INSIDE the image (matching uid) before deleting it.
  if docker image inspect "$IMG" >/dev/null 2>&1; then
    docker run --rm --entrypoint sh -v "$W:/w" "$IMG" \
      -c 'rm -rf /w/* /w/.[!.]* /w/..?* 2>/dev/null || true' >/dev/null 2>&1 || true
  fi
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  rm -rf "$W" 2>/dev/null || true
  if [ -d "$W" ]; then
    echo "WARN: cleanup could not fully remove ${W}" >&2
    [ "$SMOKE_OK" = 1 ] && { echo "FAIL: smoke passed but left files behind" >&2; exit 1; }
  fi
}
trap cleanup EXIT
die() { echo "FAIL: $1"; [ -n "${2:-}" ] && { echo "--- output ---"; echo "$2" | tail -40; }; exit 1; }

PW_OCI="smoke-oci-pw-$$"
PW_CM="smoke-cm-pw-$$"
PW_NX="smoke-nx-pw-$$"
HOST="example.invalid"

echo "==> build plugin image (--no-cache, same Dockerfile/context as the official build)"
docker build --no-cache -t "$IMG" "$HERE" >/dev/null

echo "==> helm is on the 3.22 line"
OUT="$(docker run --rm --entrypoint helm "$IMG" version --short 2>&1)" || die "helm version failed" "$OUT"
grep -qE '^v3\.22\.' <<<"$OUT" || die "helm is not on the 3.22 line" "$OUT"

echo "==> stage a minimal chart in the workspace"
mkdir -p "$W/templates"
printf 'apiVersion: v2\nname: smoke-chart\nversion: 0.0.1\n' >"$W/Chart.yaml"

run() { # run <extra -e ...> : sets RC + OUT
  set +e
  OUT="$(docker run --rm -v "$W:/workspace" -w /workspace "$@" "$IMG" 2>&1)"; RC=$?
  set -e
}
assert_reached() { # assert_reached <label> <secret-that-must-not-leak>
  [ "$RC" -ne 0 ] || die "$1: expected a non-zero failure" "$OUT"
  grep -q "==> packaged " <<<"$OUT" || die "$1: helm package did not run" "$OUT"
  grep -qiE "could not resolve host|no such host|lookup ${HOST}" <<<"$OUT" \
    || die "$1: expected the deterministic ${HOST} DNS failure" "$OUT"
  if grep -qiE 'unknown (flag|command|shorthand)|Error: accepts|requires at least|invalid argument|unknown sub' <<<"$OUT"; then
    die "$1: PARSE error — the helm/curl surface changed" "$OUT"
  fi
  # output-level leak check only (argv-level no-leak is proven by entrypoint_test.sh).
  [ -n "$2" ] && grep -q "$2" <<<"$OUT" && die "$1: secret leaked into the log" "$OUT" || true
}

# OCI is two distinct paths — login fails fast under set -e, so push would never
# run in a single creds case. Exercise them separately.
echo "==> oci LOGIN (with creds) reaches the registry; password not leaked"
run -e PLUGIN_BACKEND=oci -e PLUGIN_OCI_REPO="oci://${HOST}/charts" \
    -e PLUGIN_USERNAME=robot -e PLUGIN_PASSWORD="$PW_OCI"
assert_reached "oci-login" "$PW_OCI"
grep -q "helm registry login ${HOST}" <<<"$OUT" || die "oci-login: did not reach registry login" "$OUT"

echo "==> oci PUSH (no creds) reaches the registry"
run -e PLUGIN_BACKEND=oci -e PLUGIN_OCI_REPO="oci://${HOST}/charts"
assert_reached "oci-push" ""
grep -q "helm push " <<<"$OUT" || die "oci-push: did not reach helm push" "$OUT"

echo "==> chartmuseum backend (curl POST) reaches the repo; password not leaked"
run -e PLUGIN_BACKEND=chartmuseum -e PLUGIN_REPO_URL="https://${HOST}/cm" \
    -e PLUGIN_USERNAME=robot -e PLUGIN_PASSWORD="$PW_CM"
assert_reached "chartmuseum" "$PW_CM"

echo "==> nexus backend (curl PUT) reaches the repo; password not leaked"
run -e PLUGIN_BACKEND=nexus -e PLUGIN_REPO_URL="https://${HOST}/nexus" \
    -e PLUGIN_USERNAME=robot -e PLUGIN_PASSWORD="$PW_NX"
assert_reached "nexus" "$PW_NX"

SMOKE_OK=1
echo "PASS: helm-push smoke"
