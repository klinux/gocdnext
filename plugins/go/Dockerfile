# gocdnext/go — go toolchain plugin for gocdnext pipelines.
#
# Contract:
#   PLUGIN_COMMAND      (required)  go subcommand + args, word-split.
#                                   Examples: "build ./...", "test -race ./...",
#                                   "test -cover -coverprofile=cover.out ./...".
#   PLUGIN_WORKING_DIR  (optional)  path under /workspace; cd'd before running go.
#                                   Defaults to /workspace (repo root).
#
# Uses the official golang image so the same plugin serves any
# go.mod project without rebuilding per-toolchain. The official image
# pins GOTOOLCHAIN=local, so a project whose go.mod requires a NEWER Go
# than this image would fail; the entrypoint therefore defaults
# GOTOOLCHAIN=auto (honors go.mod's toolchain directive, downloading on
# demand into the tar'd $GOMODCACHE) — set `toolchain: local` to keep the
# old hermetic behavior. Bump this base image to track the current stable
# Go so the common case needs no toolchain download.
FROM golang:1.26-alpine

# git for `go get` / VCS-fetched deps; ca-certificates for module
# proxy TLS; make for projects whose test target leans on it.
# gcc + musl-dev so cgo is available — `go test -race` and any
# project that imports a cgo dep both refuse to build without a C
# toolchain, and alpine images ship without one by default
# (CGO_ENABLED falls back to 0). Adding ~50MB to the image is
# cheaper than telling users their race-detector tests need a
# different plugin.
RUN apk add --no-cache git ca-certificates make gcc musl-dev

COPY entrypoint.sh /usr/local/bin/gocdnext-go
RUN chmod +x /usr/local/bin/gocdnext-go

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-go"]
