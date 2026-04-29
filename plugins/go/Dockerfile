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
# go.mod project without rebuilding per-toolchain. Operators who
# need to pin a specific toolchain can set `uses: gocdnext/go@v1`
# and that maps to whatever the catalog ships as v1.
FROM golang:1.25-alpine

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
