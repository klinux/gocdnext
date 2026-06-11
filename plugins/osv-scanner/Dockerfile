# gocdnext/osv-scanner — dependency vulnerability scanner (SCA)
# for gocdnext pipelines. Complements trivy: osv-scanner reads
# LOCKFILES across ecosystems (go.mod, package-lock.json,
# poetry.lock, Cargo.lock, …) against the OSV.dev database.
#
# Contract:
#   PLUGIN_DIR          (optional)  scan root, default "." (recursive).
#   PLUGIN_FORMAT       (optional)  sarif | json | table, default sarif.
#   PLUGIN_REPORT_FILE  (optional)  default osv-report.<format>.
#   PLUGIN_FAIL_ON      (optional)  any | none, default any.
#   PLUGIN_CONFIG       (optional)  osv-scanner.toml path (ignores /
#                                   baseline equivalent).
#
# Built from source with a pinned tagged version — `go install` of
# a module version is content-addressed via the Go checksum
# database, which beats trusting a registry-side image tag (same
# rationale as the goose plugin).
FROM golang:1.25-alpine AS build
RUN CGO_ENABLED=0 go install github.com/google/osv-scanner/v2/cmd/osv-scanner@v2.0.0

FROM alpine:3.21
RUN apk add --no-cache ca-certificates jq
COPY --from=build /go/bin/osv-scanner /usr/local/bin/osv-scanner
COPY entrypoint.sh /usr/local/bin/gocdnext-osv-scanner
RUN chmod +x /usr/local/bin/gocdnext-osv-scanner

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-osv-scanner"]
