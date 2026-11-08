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
#   PLUGIN_FAIL_ON_NO_SOURCES (optional) true | false, default false. false =
#                                   "no package sources found" is a clean pass
#                                   (emits an empty SARIF); true = fail (exit 2).
#   PLUGIN_CONFIG       (optional)  osv-scanner.toml path (ignores /
#                                   baseline equivalent).
#
# Built from source with a pinned tagged version — `go install` of
# a module version is content-addressed via the Go checksum
# database, which beats trusting a registry-side image tag (same
# rationale as the goose plugin).
#
# osv-scanner bumped v2.0.0 -> v2.7.0: v2.0.0 (built on go1.25) bundled grpc
# v1.71.0 with CVE-2026-33186 (CRITICAL); v2.7.0 ships grpc >=1.79.3. Builder
# golang:1.25 -> 1.27 (pinned by digest, patched stdlib) and runtime alpine:3.21
# -> 3.24.4 (pinned) + apk upgrade (clears the OpenSSL/musl/zlib OS HIGHs). Trivy
# --pkg-types os,library (DB 2026-11-07): 0 CRITICAL; 1 HIGH remains in the
# osv-scanner binary (golang.org/x/net) — upstream-bound (v2.7.0 is the latest
# release), tracked by the weekly gate.
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
RUN CGO_ENABLED=0 go install github.com/google/osv-scanner/v2/cmd/osv-scanner@v2.7.0

FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates jq
COPY --from=build /go/bin/osv-scanner /usr/local/bin/osv-scanner
COPY entrypoint.sh /usr/local/bin/gocdnext-osv-scanner
RUN chmod +x /usr/local/bin/gocdnext-osv-scanner

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-osv-scanner"]
