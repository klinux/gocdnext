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
cat >"${WS}/openapi.yaml" <<YAML
openapi: 3.0.0
info:
  title: nuclei-smoke
  version: "1.0.0"
servers:
  - url: http://placeholder.invalid/api
paths:
  /:
    get:
      summary: root
      responses:
        "200":
          description: ok
  /health:
    get:
      summary: health
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
# Proves yq parsed the spec, the entrypoint rewrote the base URL to the target,
# and nuclei ran in OpenAPI input mode — not a parse/spec error.
echo "${specout}" | grep -qi 'nuclei (spec openapi) against' \
  || { echo "FAIL: spec path did not enter OpenAPI mode"; echo "${specout}" | tail -20; exit 1; }
echo "${specout}" | grep -qiE 'templates loaded|new templates added|Templates Total|Scan completed' \
  || { echo "FAIL: OpenAPI-driven scan did not run"; echo "${specout}" | tail -20; exit 1; }

echo "PASS: nuclei smoke"
