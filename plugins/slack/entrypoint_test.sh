#!/usr/bin/env bash
# Unit tests for the slack plugin. It's a compiled Go binary, so its "mock"
# coverage is a Go test (webhook-token scrubbing), not a shell entrypoint mock.
# Runs `go test` in the SAME pinned build toolchain the image uses, so CI
# exercises the scrub logic offline (the module has no third-party deps).
#
# Convention-named so plugins.yml runs it as a pre-publish gate. Needs docker.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GO_IMG="golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18"

echo "==> go test (webhook-scrub / sanitizer unit tests)"
docker run --rm -v "${HERE}:/src" -w /src "${GO_IMG}" go test ./...

echo "PASS: slack unit tests"
