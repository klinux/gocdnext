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
FROM semgrep/semgrep:1.96.0

RUN apk add --no-cache jq

COPY entrypoint.sh /usr/local/bin/gocdnext-semgrep
RUN chmod +x /usr/local/bin/gocdnext-semgrep

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-semgrep"]
