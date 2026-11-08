# gocdnext/buf — wrap the bufbuild CLI so pipelines call
# `lint`, `breaking`, `generate` etc. without re-pinning the
# image per-job.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_COMMAND      (required)  buf subcommand + args, word-split.
#                                   Examples: "lint", "breaking --against .git#branch=main",
#                                   "generate".
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
# Base bumped bufbuild/buf:1.47 -> 1.75.0 (pinned by digest): 1.47 was built on an
# old Go + grpc (Go-stdlib CVE-2025-68121, grpc CVE-2026-33186) on an Alpine with
# the OpenSSL libcrypto3/libssl3 CVE-2026-31789 (CRITICAL). 1.75.0's binary is
# built on go1.27.3, on Alpine 3.24; `apk upgrade` clears the OS OpenSSL CRITICAL.
# The FROM digest pins the base layer, not the upgraded set; the weekly Trivy gate
# catches drift. buf's CLI surface (lint/breaking/generate) is unchanged.
FROM bufbuild/buf:1.75.0@sha256:adf82c7170be6629ec81dc907921ac1699407cd11c4f7fd7521b13c74152f849

# bufbuild/buf is alpine-based; sh is already there but we want
# git for `breaking --against` references and ca-certificates
# for module proxy TLS.
USER root
RUN apk upgrade --no-cache && apk add --no-cache git ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-buf
RUN chmod +x /usr/local/bin/gocdnext-buf

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-buf"]
