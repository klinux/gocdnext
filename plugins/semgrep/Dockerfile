# gocdnext/semgrep — SAST scanner plugin for gocdnext pipelines.
#
# Contract:
#   PLUGIN_CONFIG       (optional)  rule source(s), default p/default.
#                                   Registry packs (p/golang,
#                                   p/owasp-top-ten), local files or
#                                   dirs; comma/space-separated.
#   PLUGIN_FAIL_ON      (optional)  error | warning | info | none —
#                                   exit-1 threshold, default error.
#   PLUGIN_REPORT_FILE  (optional)  SARIF output, default semgrep.sarif
#                                   (attach via the job's artifacts:).
#   PLUGIN_BASELINE_COMMIT (optional)  only NEW findings vs the commit.
#
# Telemetry is OFF (--metrics=off) — a CI scanner phoning home is
# an opt-in, never a default. jq rides along for the SARIF-based
# severity gate (full report always written; the threshold only
# decides the exit code, so the artifact never loses findings).
#
# Pinned by digest (1.184.0 on alpine 3.23.8, Python 3.12). The bump
# from 1.96.0 clears all three CRITICALs WITHOUT any pip/apk surgery —
# they ride in the upstream image:
#   - OS OpenSSL (libcrypto3/libssl3 CVE-2026-31789): alpine 3.23.8
#     already ships the patched OpenSSL (3.5.9-r0); `apk upgrade`
#     keeps it fresh.
#   - certifi (GHSA-6q48-6c7w-9gw2): 1.184.0 bundles certifi
#     2026.10.23 (>= the 2026.10.18 fix) in semgrep's own venv.
# semgrep is a scanner, so a version bump is desirable (newer engine +
# rules); the wrapper CONTRACT (SARIF shape, --metrics=off,
# --baseline-commit, the jq severity gate) is unchanged and smoke-proven.
# apk upgrade pulls Alpine's mutable repos; the FROM digest pins the base
# layer and the weekly Trivy gate catches drift (hence no_cache:true).
FROM semgrep/semgrep:1.184.0@sha256:cc1b68266f267878b39739373ea9985b9a78e5d64497dd875c8de100b22695ed

USER root
RUN apk upgrade --no-cache && apk add --no-cache jq

COPY entrypoint.sh /usr/local/bin/gocdnext-semgrep
RUN chmod +x /usr/local/bin/gocdnext-semgrep

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-semgrep"]
