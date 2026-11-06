#!/usr/bin/env bash
# Real-image gate for the bundled and dynamically selected Terraform versions.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="gocdnext-plugin-terraform:smoke-$$"
WORK="$(mktemp -d)"
cleanup() {
  docker image rm -f "${IMG}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

echo "==> build (same Dockerfile/context as the official build)"
docker build -t "${IMG}" "${HERE}" >/dev/null

out="$(docker run --rm -e PLUGIN_COMMAND=version "${IMG}" 2>&1)"
printf '%s\n' "${out}" | grep -q 'Terraform v1.17.1' || {
  echo "FAIL: latest/default did not use bundled 1.17.1"; echo "${out}"; exit 1;
}

# Run as a non-root uid too: the runtime cache is under /tmp and must remain
# writable. This also exercises the real HashiCorp download + SHA verification.
out="$(docker run --rm --user 12345:12345 \
  -e PLUGIN_COMMAND=version -e PLUGIN_VERSION=1.8.5 "${IMG}" 2>&1)"
printf '%s\n' "${out}" | grep -q 'Terraform v1.8.5' || {
  echo "FAIL: version=1.8.5 was not selected"; echo "${out}"; exit 1;
}
printf '%s\n' "${out}" | grep -q 'SHA256 hash matched' || {
  echo "FAIL: exact-version download was not checksum-verified"; echo "${out}"; exit 1;
}

if docker run --rm -e PLUGIN_COMMAND=version -e PLUGIN_VERSION=latest-allowed "${IMG}" >/dev/null 2>&1; then
  echo "FAIL: non-exact version selector should be rejected"
  exit 1
fi

echo "PASS: terraform smoke"
