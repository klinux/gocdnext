---
title: Required checks for merge
description: Pick which pipelines must be green to merge a PR — gocdnext writes the GitHub ruleset.
---

Mark specific pipelines as **required to merge a pull request**. gocdnext writes
a dedicated **GitHub ruleset** on the repo requiring exactly those pipelines'
checks; GitHub blocks the merge until they pass. gocdnext does **not** merge —
it configures the ruleset; GitHub enforces it.

Admin-only, per project: *Project → Settings → Required checks*.

## How it works

1. You pick the required pipelines (only those that run on pull requests are
   selectable — see below).
2. gocdnext creates/updates a ruleset named **`gocdnext-required-checks-<project>`**
   on the repo, requiring each pipeline's commit-status context
   `ci/gocdnext/<project>/<pipeline>`. The name carries the project slug, so two
   projects bound to the same repo each own a separate ruleset and never clobber
   each other.
3. A PR to the default branch can't merge until every required pipeline is green.

Because it is a **dedicated ruleset**, it stacks with any other rules you already
have — gocdnext never touches your existing branch protection or other rulesets.
Clear all required pipelines and gocdnext tears its ruleset back down.

## Prerequisites

- **GitHub App permission `Administration: write`.** Writing rulesets needs it;
  the App doesn't request it by default, so an org admin must re-approve the App
  once. Without it, saving still stores your choice but the sync fails with an
  actionable *"re-approve the App"* message (nothing is half-applied). See
  [App permissions](/install/webhooks/#required-app-permissions).
- **Commit-status reporting on.** The required context is the commit status, so
  the project's *check reporting* must be `both` or `commit_status` (the
  default is `both`). A project set to `check_run` only can't be gated this way —
  the UI blocks it and tells you to switch.

## Which pipelines are eligible

A pipeline is offered (and accepted) as required **only** when it will reliably
report a check for every PR to the default branch. Concretely, it must have a git
material that:

- fires on `pull_request` (`events: [..., pull_request]`),
- points at **the project's own repo** and the **default branch** (matched by the
  same canonical fingerprint the webhook uses — a material on another repo or
  another branch is excluded), and
- has **no path filter** (`when.paths`) — a path-scoped pipeline is skipped for
  PRs that don't touch its paths, so its check wouldn't post for those.

Anything outside this set would **deadlock the merge** (GitHub waits forever for a
check that never arrives), so it isn't selectable. A path-scoped pipeline simply
won't appear in the list — that's expected, not a bug.

If you later rename/remove a required pipeline, change its events/branch, or add a
path filter, gocdnext re-syncs the ruleset on the next project apply, dropping any
pipeline that is no longer eligible.

## GitHub only (for now)

v1 writes GitHub repository rulesets. GitLab ("pipelines must succeed" / protected
branches) and Bitbucket (merge checks) are not yet wired — the setting is hidden
for non-GitHub projects.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Save shows **"re-approve the App"** | App lacks `Administration: write` | Grant it, have the org admin approve, then **Retry** |
| A PR is **stuck**, waiting on a check that never posts | a required pipeline doesn't run on the PR, or reporting is `check_run`-only | remove that pipeline from the required set, or switch reporting to include commit statuses |
| Ruleset disappeared | someone deleted it in GitHub | Save again — gocdnext recreates it |
