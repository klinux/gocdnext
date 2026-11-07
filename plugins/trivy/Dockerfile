# gocdnext/trivy — scan images, filesystems, or repos for CVEs +
# secret leaks. Wraps Aqua's official CLI.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_SCAN_TYPE       (optional)  image | fs | repo | config.
#                                      Defaults to "fs" (scans the workspace).
#   PLUGIN_TARGET          (optional)  what to scan:
#                                        image: "ghcr.io/org/app:v1"
#                                        fs:    "./" (defaults to "/workspace")
#                                        repo:  "https://github.com/org/repo"
#                                        config: "./k8s/"
#                                      Defaults differ per scan_type.
#   PLUGIN_SEVERITY        (optional)  comma-separated severity filter:
#                                      LOW,MEDIUM,HIGH,CRITICAL.
#                                      Defaults to HIGH,CRITICAL so CI
#                                      isn't drowned in LOW noise.
#   PLUGIN_EXIT_CODE       (optional)  exit code when any finding matches the
#                                      severity filter. Default 1 (fail CI).
#                                      Set to 0 for advisory-only scans.
#   PLUGIN_IGNORE_UNFIXED  (optional)  "true" to skip CVEs without a fix
#                                      available. Default "true" — reduces
#                                      noise on libraries the upstream
#                                      hasn't patched yet.
#   PLUGIN_SKIP_DIRS       (optional)  comma-separated dirs/globs to
#                                      exclude from an fs/repo scan
#                                      (e.g. "node_modules,vendor" —
#                                      build-time deps that never ship).
#                                      Forwarded as trivy --skip-dirs.
#   PLUGIN_FORMAT          (optional)  table | json | sarif | template.
#                                      Default table for human logs.
#   PLUGIN_OUTPUT          (optional)  write report to this path (relative
#                                      to workspace). Empty = stdout only.
# aquasec/trivy bumped 0.56.2 -> 0.76.2 (pinned by digest). 0.56.2 carried 5
# CRITICAL: Alpine OpenSSL (libcrypto3/libssl3 CVE-2026-31789) and the trivy Go
# binary's deps (go-git CVE-2025-21613, grpc CVE-2026-33186, Go-stdlib
# CVE-2025-68121). 0.76.2's binary bundles the patched deps and `apk upgrade`
# clears the OS packages -> Trivy on the image: 0 CRITICAL. Bumping the binary
# also moves the EMBEDDED misconfig checks forward together (the entrypoint uses
# --skip-check-update for `config` scans, so checks track the binary, not a
# drifting registry bundle). Opts into `no_cache` (plugin.yaml) for OS currency.
# NOTE: `apk upgrade` pulls Alpine's MUTABLE repos — the FROM digest pins the base
# layer, not the upgraded set; the weekly Trivy gate catches drift.
FROM aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb

RUN apk upgrade --no-cache && apk add --no-cache bash

COPY entrypoint.sh /usr/local/bin/gocdnext-trivy
RUN chmod +x /usr/local/bin/gocdnext-trivy

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-trivy"]
