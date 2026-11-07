# gocdnext/slack — reference notification plugin (pure-stdlib Go).
#
# The binary has no third-party deps, so its only vuln surface is the Go stdlib
# baked in by the toolchain. Build stage bumped golang:1.23 -> 1.27 (pinned by
# digest, go1.27.3) so the stdlib is patched (Go-stdlib CVE-2025-68121 et al.);
# runtime base bumped alpine:3.20 (EOL 2026-04-01) -> 3.24.4 (pinned) + apk
# upgrade. The FROM digest pins the base layer, not the upgraded set; the weekly
# Trivy gate catches OS drift.
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/slack-plugin .

FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates
COPY --from=build /out/slack-plugin /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/slack-plugin"]
