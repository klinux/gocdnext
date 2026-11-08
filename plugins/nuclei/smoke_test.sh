#!/usr/bin/env bash
# Real smoke for the nuclei plugin — runs the PINNED IMAGE (not the mock), so a
# breaking nuclei release that renames a flag or drops the baked templates fails
# here, BEFORE the publishing build. Convention-named so plugins.yml can run it.
#
# Fast + deterministic: a flag-grep + one tiny scan against a throwaway HTTP
# container on a dedicated docker network. Unique names + trap cleanup so a
# failure (or a parallel rerun) never leaves dirty state.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-nuclei:smoke-$$"
NET="gocdnext-nuclei-smoke-$$"
TGT="nuclei-smoke-target-$$"
WS="$(mktemp -d)"

cleanup() {
  docker rm -f "${TGT}" >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
  docker image rm -f "${IMG}" >/dev/null 2>&1 || true
  rm -rf "${WS}" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> build --no-cache (match the official no_cache publish build)"
docker build --no-cache -t "${IMG}" "${HERE}" >/dev/null

echo "==> flag-drift check against the pinned binary"
help="$(docker run --rm --entrypoint nuclei "${IMG}" -h 2>&1)"
for f in -sarif-export -jsonl-export -omit-raw -redact -no-interactsh \
         -disable-update-check -disable-redirects -type -input-mode -list -severity; do
  echo "${help}" | grep -qE -- "(^|[ ,])${f}([ ,]|$|=)" || { echo "FAIL: pinned nuclei missing ${f}"; exit 1; }
done

echo "==> micro-scan against a throwaway target on a dedicated network"
docker network create "${NET}" >/dev/null
docker run -d --rm --name "${TGT}" --network "${NET}" nginx:alpine >/dev/null
out="$(docker run --rm --network "${NET}" \
  -e PLUGIN_TARGET="http://${TGT}:80" \
  -e PLUGIN_TAGS="tech" \
  -e PLUGIN_EXIT_CODE="0" \
  -e PLUGIN_READY_TIMEOUT="30" \
  "${IMG}" 2>&1)"
echo "${out}" | tail -5

# Templates must actually load (proves the baked set is found via -t /opt/...).
echo "${out}" | grep -qiE 'templates loaded|new templates added|Templates Total' \
  || { echo "FAIL: no templates loaded — baked templates not found at runtime"; exit 1; }

echo "==> API-driven scan from a REAL OpenAPI spec (base URL rewritten to the target)"
# The spec's server points at a host that does NOT resolve. The ONLY way the
# unique probe path can reach the target's access log is if the entrypoint
# rewrote the spec's server to PLUGIN_TARGET (the thing under test) AND nuclei
# actually ran the spec-derived request against it. Printing "nuclei (spec ...)"
# or "Scan completed" does NOT prove that — a no-op yq would still print both and
# exit 0 (nuclei just can't resolve placeholder.invalid). The access log does.
PROBE="/__gocdnext_openapi_probe_$$"
cat >"${WS}/openapi.yaml" <<YAML
openapi: 3.0.0
info:
  title: nuclei-smoke
  version: "1.0.0"
servers:
  - url: http://placeholder.invalid
paths:
  ${PROBE}:
    get:
      summary: probe
      responses:
        "200":
          description: ok
YAML
specout="$(docker run --rm --network "${NET}" -v "${WS}:/workspace" -w /workspace \
  -e PLUGIN_TARGET="http://${TGT}:80" \
  -e PLUGIN_SPEC="openapi.yaml" \
  -e PLUGIN_EXIT_CODE="0" \
  -e PLUGIN_READY_TIMEOUT="30" \
  "${IMG}" 2>&1)"
echo "${specout}" | tail -5
echo "${specout}" | grep -qi 'nuclei (spec openapi) against' \
  || { echo "FAIL: spec path did not enter OpenAPI mode"; echo "${specout}" | tail -20; exit 1; }

# The preflight only touches "/", so the probe path in the target's access log
# proves the rewrite happened and the spec request actually hit the target.
if ! docker logs "${TGT}" 2>&1 | grep -q -- "${PROBE}"; then
  echo "FAIL: probe path ${PROBE} never reached the target — spec rewrite not proven"
  echo "--- target access log ---"; docker logs "${TGT}" 2>&1 | tail -20
  echo "--- scan output ---"; echo "${specout}" | tail -20
  exit 1
fi

echo "PASS: nuclei smoke"
