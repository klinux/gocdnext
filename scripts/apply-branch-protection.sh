#!/usr/bin/env bash
#
# apply-branch-protection.sh — declarative GitHub Rulesets for main.
#
# Anchors the trunk-based-release.md "Separation of duties" section
# at the repo's enforcement layer. Until now CODEOWNERS named WHO
# reviews each path; this script enables the GitHub-level rule that
# MAKES code-owner approval mandatory (and blocks the "I'll just
# merge my own PR" path) along with required CI status checks.
#
# Idempotent: list the named ruleset → update if it exists, create
# if it doesn't. Re-runs converge on the same state. Run after every
# meaningful change to the policy below.
#
# Usage:
#   gh auth status                         # confirm authenticated
#   scripts/apply-branch-protection.sh     # apply / refresh
#
# Verify after apply:
#   gh api "repos/${OWNER}/${REPO}/rulesets" \
#     --jq '.[] | select(.name=="main-protection") | {id, enforcement, target}'
#
# Why Rulesets and not the classic /branches/main/protection API:
# Rulesets are the newer surface, support multi-branch scope, can
# be audited via the standard ruleset insights tab, and unify with
# the org-level policy story when this repo eventually moves into
# an org. The classic API still works but is in maintenance — every
# new flag (allowed_merge_methods, automatic_copilot_*) lands here
# first.

set -euo pipefail

OWNER="${OWNER:-klinux}"
REPO="${REPO:-gocdnext}"
RULESET_NAME="${RULESET_NAME:-main-protection}"

# Required status checks. The contexts MUST match the job names
# emitted by .github/workflows/ci.yml — GitHub creates one check
# per job, and the context string is the job key (`go`, `web`)
# unless the job declares a `name:` override (none do today).
#
# Tighten this list when CI grows new jobs whose green-ness is
# load-bearing for "ready to merge". Don't add jobs that are
# advisory only (e.g. flaky perf benchmarks) — a required check
# that's allowed to fail kills the contract.
REQUIRED_CHECKS=("go" "web")

# Build the JSON ruleset payload. Each rule below carries its
# justification inline because future-you (or a reviewer) will
# wonder why each flag is on.
payload() {
  local checks_json
  checks_json=$(printf '%s\n' "${REQUIRED_CHECKS[@]}" \
    | jq -R '{context: .}' \
    | jq -s '.')

  cat <<EOF
{
  "name": "${RULESET_NAME}",
  "target": "branch",
  "enforcement": "active",
  "conditions": {
    "ref_name": {
      "include": ["refs/heads/main"],
      "exclude": []
    }
  },
  "bypass_actors": [],
  "rules": [
    {
      "type": "deletion"
    },
    {
      "type": "non_fast_forward"
    },
    {
      "type": "pull_request",
      "parameters": {
        "required_approving_review_count": 1,
        "dismiss_stale_reviews_on_push": true,
        "require_code_owner_review": true,
        "require_last_push_approval": true,
        "required_review_thread_resolution": false,
        "allowed_merge_methods": ["squash"]
      }
    },
    {
      "type": "required_status_checks",
      "parameters": {
        "strict_required_status_checks_policy": true,
        "required_status_checks": ${checks_json}
      }
    },
    {
      "type": "required_linear_history"
    }
  ]
}
EOF
}

# Per-flag rationale (kept here, not inlined in JSON which doesn't
# support comments):
#
# - deletion / non_fast_forward / required_linear_history: main
#   never accepts force-push, never gets deleted, never grows a
#   merge commit. Combines with squash-only merge below so the
#   history stays bisectable for `git log --oneline` walks.
#
# - bypass_actors: []  — nobody bypasses. Solo maintainer state
#   today; when contributors come in, this stays empty. The
#   "bypass for owner" temptation is the path back to "I trust
#   myself" territory the trunk-doc explicitly warns against.
#
# - required_approving_review_count: 1 — paired with
#   require_code_owner_review, this means CODEOWNERS must approve.
#   GitHub natively forbids self-approval of one's own PR; combined
#   with code-owner mandate, a PR author who IS the only relevant
#   code owner needs a second human. Tightens "separation of
#   duties" naturally as the team grows: nothing to reconfigure
#   when a second maintainer joins.
#
# - dismiss_stale_reviews_on_push: true — a force-push to a PR
#   after approval invalidates the approval. Closes the
#   "reviewed-then-rewrote" attack window.
#
# - require_code_owner_review: true — fires CODEOWNERS for the
#   touched paths. /.github/workflows, /server/migrations,
#   /charts, /proto, /server/internal/{secrets,auth,api/authapi,
#   api/admin}, /server/internal/webhook are all flagged owned
#   by @klinux today, so any change there demands the owner's
#   review.
#
# - require_last_push_approval: true — the SAME approval can't
#   carry over a new push from the author. Defence-in-depth on
#   top of dismiss_stale_reviews.
#
# - required_review_thread_resolution: false — kept off so
#   nit-thread reviewers don't block urgent fixes. Toggle on if
#   PRs grow a habit of merging with unresolved security threads.
#
# - allowed_merge_methods: ["squash"] — squash is the only one
#   that keeps main's history linear AND retains the PR's title
#   as the merge commit subject (Conventional Commits land
#   cleanly). Merge commits would muddy `git log --oneline`;
#   rebase merges drop the PR boundary entirely.
#
# - strict_required_status_checks_policy: true — PR must be up
#   to date with main before merging. Catches "I rebased and
#   broke things" before the merge lands, not after.
#
# - required_status_checks: ["go", "web"] — see top-of-file
#   array. CI workflow's job keys.

# Find existing ruleset by name; capture id when present.
existing_id=$(gh api "repos/${OWNER}/${REPO}/rulesets" \
  --jq ".[] | select(.name==\"${RULESET_NAME}\") | .id" \
  | head -n1)

body=$(payload)

if [[ -n "${existing_id}" ]]; then
  echo "↻ Updating ruleset ${RULESET_NAME} (id=${existing_id})"
  echo "${body}" | gh api \
    --method PUT \
    -H "Accept: application/vnd.github+json" \
    "repos/${OWNER}/${REPO}/rulesets/${existing_id}" \
    --input -
else
  echo "✚ Creating ruleset ${RULESET_NAME}"
  echo "${body}" | gh api \
    --method POST \
    -H "Accept: application/vnd.github+json" \
    "repos/${OWNER}/${REPO}/rulesets" \
    --input -
fi

echo
echo "Verify with:"
echo "  gh api 'repos/${OWNER}/${REPO}/rulesets' --jq '.[] | select(.name==\"${RULESET_NAME}\") | {id, enforcement, target}'"
