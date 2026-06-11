# gocdnext/goose — goose migration runner for gocdnext pipelines.
# The Go-ecosystem counterpart of the flyway/liquibase plugins —
# and the tool gocdnext's own schema is managed with (dogfooding).
#
# Contract:
#   PLUGIN_COMMAND   (required)  status | validate | up.
#   GOOSE_DBSTRING   (required)  DSN via the job's `secrets:` —
#                                goose reads its own env natively.
#   PLUGIN_DRIVER    (optional)  default postgres.
#   PLUGIN_DIR       (optional)  migrations dir, default ./migrations.
#
# Built from source with a pinned version (the same line the
# gocdnext server pins in go.mod) — `go install` of a tagged module
# version is content-addressed via the Go module checksum database,
# which beats trusting a registry-side image tag.
FROM golang:1.25-alpine AS build
RUN CGO_ENABLED=0 go install github.com/pressly/goose/v3/cmd/goose@v3.27.0

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /go/bin/goose /usr/local/bin/goose
COPY entrypoint.sh /usr/local/bin/gocdnext-goose
RUN chmod +x /usr/local/bin/gocdnext-goose

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-goose"]
