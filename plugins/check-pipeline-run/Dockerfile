# gocdnext/check-pipeline-run — preflight check against the
# gocdnext REST API. Confirms a target pipeline produced a
# terminal-success run matching the operator's filter (tag,
# commit revision) before a downstream step (typically a
# prod-deploy gate) proceeds.
#
# Closes the trunk-based-release recipe's "Per-tag preflight via
# API" limitation — replaces the inline curl + jq script in
# prod.yaml's preflight job with a typed plugin + structured
# outputs the downstream consumes.
#
# Two modes:
#   - single-shot: query once, fail loud if no match. Useful when
#     the upstream run is GUARANTEED to be terminal by the time
#     this fires (operator workflow: open prod.yaml after the
#     release/stage smoke went green).
#   - poll: query every interval seconds up to max-attempts.
#     Useful when the prod deploy is queued right after the
#     release pipeline and the upstream might still be running.
#     Default: single-shot.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_API_URL          (required)  gocdnext base URL, e.g.
#                                       `https://gocdnext.example.com`.
#                                       No trailing slash; the plugin
#                                       appends `/api/v1/...` paths.
#   PLUGIN_API_TOKEN        (required)  `gnk_...` API token. Use
#                                       `secrets:` to pipe it in —
#                                       the plugin writes it to a
#                                       curl --config tempfile so it
#                                       never lands on argv.
#   PLUGIN_PROJECT          (required)  project slug to query
#                                       (e.g. `acme-org`).
#   PLUGIN_PIPELINE         (required)  pipeline name to match
#                                       (e.g. `release`). Filtered
#                                       inside the project's recent
#                                       runs list.
#   PLUGIN_TAG              (optional)  match runs whose
#                                       `cause_detail.tag_name`
#                                       equals this value. EXACTLY
#                                       one of PLUGIN_TAG or
#                                       PLUGIN_REVISION must be set
#                                       — blank (matches any) and
#                                       both (silent AND) are both
#                                       rejected with exit 2.
#   PLUGIN_REVISION         (optional)  match runs whose primary
#                                       revision equals this commit
#                                       SHA (full 40 hex or short —
#                                       prefix-match). Mutually
#                                       exclusive with PLUGIN_TAG.
#   PLUGIN_EXPECTED_STATUS  (optional)  default `success`. Use
#                                       `success,canceled` (comma-
#                                       sep) on rare flows that
#                                       accept multiple terminals.
#   PLUGIN_MAX_AGE          (optional)  default `7d`. Reject matches
#                                       whose finished_at is older
#                                       than this. Format:
#                                       `<N>{m|h|d}`. Set to `0`
#                                       to disable.
#   PLUGIN_POLL_ATTEMPTS    (optional)  default `1`. When > 1, the
#                                       plugin re-queries between
#                                       attempts so a recently-
#                                       dispatched upstream gets
#                                       picked up without a manual
#                                       re-run downstream.
#   PLUGIN_POLL_INTERVAL    (optional)  default `30s`. Seconds
#                                       between poll attempts.
#                                       Capped at 600s to bound
#                                       a hostile / mistyped value.
#   PLUGIN_RUNS_LIMIT       (optional)  default + cap = `100`.
#                                       The server-side project
#                                       detail handler silently
#                                       caps `?runs=` at 100 — the
#                                       plugin mirrors that so the
#                                       contract is not a lie. If
#                                       the server cap is raised,
#                                       lift this in lockstep.
#   PLUGIN_OUTPUT           (optional)  default
#                                       `.gocdnext/preflight.env`.
#                                       Workspace-relative output
#                                       file. ALSO written to
#                                       $GOCDNEXT_OUTPUT_FILE when
#                                       set by the agent (v0.11+).
#
# Output file shape (shell-sourceable):
#   RUN_ID='<uuid>'
#   RUN_URL='https://gocdnext.example.com/runs/<uuid>'
#   REVISION='<full commit SHA>'
#   COUNTER='<run counter int>'
#   FINISHED_AT='<RFC3339 timestamp>'
#
# Exit codes:
#   0 — match found, output file written.
#   1 — no match (after all poll attempts). The downstream gate
#       fails — this is the LOAD-BEARING failure mode that
#       prevents a prod deploy from proceeding when the release
#       didn't pass stage smoke.
#   2 — input validation error (missing required, bad charset,
#       conflicting filter).
#   3 — API unreachable, auth rejected, response shape unexpected,
#       OR a per-run detail fetch failed during the scan, OR the
#       matched run's fields didn't pass the shell-safe charset
#       validation before being written to the output file. Distinct
#       from 1 so the operator runbook knows whether to investigate
#       the gocdnext API/network (3) or the upstream pipeline (1).
#       If at least one API error happens during the poll AND no
#       run matched, the plugin exits 3 (not 1) — a degraded API
#       call could have hidden a legitimate match.

FROM alpine:3.20

# bash: %-prefixed expansion semantics + IFS quirks rely on bash;
#       ash would silently surprise on edge cases.
# curl: REST call.
# jq:   JSON filter against the REST response — pulling
#       cause_detail.tag_name out of /api/v1/runs/{id} response
#       without ad-hoc string carving.
# ca-certificates: HTTPS to a production gocdnext deploy.
# coreutils: GNU base64 (-w0) + date (--date / --iso-8601) — the
#            BusyBox flavours wrap base64 + reject ISO-8601
#            timestamp parsing the API returns.
RUN apk add --no-cache bash ca-certificates curl jq coreutils

COPY entrypoint.sh /usr/local/bin/gocdnext-check-pipeline-run
RUN chmod +x /usr/local/bin/gocdnext-check-pipeline-run

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-check-pipeline-run"]
