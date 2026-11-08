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
# Built from source with a pinned version — `go install` of a tagged module
# version is content-addressed via the Go module checksum database, which beats
# trusting a registry-side image tag.
#
# goose bumped v3.27.0 -> v3.29.0: v3.27.0 bundled vulnerable deps (pgx/v5
# CVE-2026-33815 + CVE-2026-33816, grpc CVE-2026-33186 — all CRITICAL); v3.29.0
# ships pgx >=5.9.0 and grpc >=1.79.3. Builder golang:1.25 -> 1.27 (pinned by
# digest, patched stdlib) and base alpine:3.21 -> 3.24.4 (pinned) + apk upgrade.
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
RUN CGO_ENABLED=0 go install github.com/pressly/goose/v3/cmd/goose@v3.29.0

FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates
COPY --from=build /go/bin/goose /usr/local/bin/goose
COPY entrypoint.sh /usr/local/bin/gocdnext-goose
RUN chmod +x /usr/local/bin/gocdnext-goose

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-goose"]
