#!/usr/bin/env bash
# Real-image gate for the official Coveralls reporter binary and LCOV parser.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-coveralls:smoke-$$"
WORK="$(mktemp -d)"
cleanup() {
  docker image rm -f "${IMG}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

mkdir -p "${WORK}/src"
printf 'const x = 1;\n' >"${WORK}/src/x.js"
printf 'TN:\nSF:src/x.js\nDA:1,1\nLF:1\nLH:1\nend_of_record\n' >"${WORK}/lcov.info"

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "${IMG}" "${HERE}" >/dev/null

version="$(docker run --rm --entrypoint coveralls "${IMG}" version)"
[ "${version}" = "0.6.22" ] || { echo "FAIL: expected coverage-reporter 0.6.22, got ${version}"; exit 1; }

# Parse a real report without network submission. This catches wrong assets,
# missing runtime libraries and format/CLI drift before publish.
out="$(docker run --rm --entrypoint coveralls \
  -e COVERALLS_REPO_TOKEN=smoke-token \
  -e COVERALLS_RUN_LOCALLY=true \
  -v "${WORK}:/workspace:ro" -w /workspace \
  "${IMG}" report lcov.info --format=lcov --dry-run --no-logo 2>&1)"
printf '%s\n' "${out}" | grep -qiE 'dry.run|coverage|report' || {
  echo "FAIL: reporter did not parse LCOV in dry-run mode"; echo "${out}"; exit 1;
}

# The entry-less parallel finaliser is a distinct command path in the official
# reporter. Exercise it without touching Coveralls' network endpoint.
docker run --rm --entrypoint coveralls \
  -e COVERALLS_REPO_TOKEN=smoke-token -e COVERALLS_RUN_LOCALLY=true \
  "${IMG}" "done" --build-number=1 --dry-run --no-logo >/dev/null 2>&1

echo "PASS: coveralls smoke"
