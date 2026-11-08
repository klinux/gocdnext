# gocdnext/gitleaks — secret scanner. Runs against the workspace
# checkout looking for accidentally-committed credentials,
# tokens, and keys. Cheap to add at the front of any pipeline
# and catches the mistake before the commit propagates further.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_PATH       (optional)  path under /workspace to scan.
#                                 Defaults to ".".
#   PLUGIN_CONFIG     (optional)  path to a gitleaks.toml config
#                                 (relative to the workspace).
#                                 Empty = gitleaks default rules.
#   PLUGIN_FORMAT     (optional)  json | sarif | csv. Default
#                                 "json" (trivial to grep).
#   PLUGIN_REPORT     (optional)  path (under workspace) to write
#                                 the report to. Pair with
#                                 artifacts.optional to attach.
#   PLUGIN_SCAN_MODE  (optional)  "dir" | "git". "dir" scans the
#                                 working tree; "git" scans the
#                                 full git history. Default "dir"
#                                 — most CI runs care about the
#                                 current revision, not
#                                 regressions buried in history.
#   PLUGIN_EXIT_CODE  (optional)  exit code when leaks are found.
#                                 Default 1 (fail CI). Set to 0
#                                 for advisory-only scans.
# Built from source (not the upstream image): the published zricethezav/gitleaks
# image bakes its binary with the vendor's Go, which lagged on a patched stdlib
# (v8.21.2 was go1.22.8 → CVE-2025-68121). Building v8.30.1 ourselves on go1.27
# clears the stdlib CVEs the vendor image can't, on a controlled, patched base.
# `go install` of a tagged version is content-addressed via the Go checksum DB
# (same rationale as the osv-scanner/goose plugins). Default rules are go:embed'd
# in the binary, so no separate config is needed; the version string is set via
# -ldflags so `gitleaks version` reports v8.30.1.
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
RUN apk add --no-cache git
RUN CGO_ENABLED=0 go install \
      -ldflags "-X github.com/zricethezav/gitleaks/v8/version.Version=v8.30.1" \
      github.com/zricethezav/gitleaks/v8@v8.30.1

FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
RUN apk upgrade --no-cache && apk add --no-cache bash git
COPY --from=build /go/bin/gitleaks /usr/bin/gitleaks

COPY entrypoint.sh /usr/local/bin/gocdnext-gitleaks
RUN chmod +x /usr/local/bin/gocdnext-gitleaks

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-gitleaks"]
