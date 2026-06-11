# gocdnext/pr-comment — post (or upsert) a comment on the PR/MR
# that triggered the run. GitHub / GitLab / Bitbucket Cloud.
#
# Contract (see entrypoint.sh for the full input set):
#   PLUGIN_BODY | PLUGIN_BODY_FILE  (exactly one)  comment content
#   PLUGIN_MARKER                   (optional)     upsert identity
#   PLUGIN_MODE                     (optional)     upsert | create
#   GITHUB_TOKEN / GITLAB_TOKEN / BITBUCKET_TOKEN (or
#   BITBUCKET_USERNAME + BITBUCKET_APP_PASSWORD)   via `secrets:`
#
# The PR/MR is resolved from CI_PULL_REQUEST_URL (stamped by all
# three webhook providers) — no repo/provider inputs needed; the
# path shape disambiguates (/pull/ vs /-/merge_requests/ vs
# /pull-requests/), which also covers self-hosted GitHub
# Enterprise and GitLab instances.
FROM alpine:3.21

RUN apk add --no-cache curl jq ca-certificates

COPY entrypoint.sh /usr/local/bin/gocdnext-pr-comment
RUN chmod +x /usr/local/bin/gocdnext-pr-comment

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-pr-comment"]
