---
title: Approval gates
description: Halt a run at a job until N members of an approver group sign off — basics, groups, quorum.
---

Approval gates pause a run at a specific job until human(s) click
*Approve* in the dashboard. Used for:

- Production deploys that need a second pair of eyes.
- Promotions across environments (staging → prod).
- Destructive operations (data migration, mass-update).
- Compliance flows (separation of duties).

## The simplest gate

```yaml
jobs:
  promote-prod:
    stage: deploy
    approval:
      description: "Promote build to production"
    image: alpine
    script: ["echo deploying"]
```

When the run reaches this job, its status flips to
`awaiting_approval`. The dashboard surfaces a banner with
*Approve / Reject* buttons. The first authenticated user with
the role to approve clicks → job dispatches → script runs.

Without `groups:` and `quorum:`, ANY authenticated user (admin,
maintainer, or viewer) can approve. The audit trail records who
clicked.

## Groups + quorum

Real production gates need:

- **Restrict who can approve** to a defined set (security team,
  release managers, etc.).
- **Require multiple approvers** so a single account compromise
  can't ship to prod.

```yaml
jobs:
  promote-prod:
    stage: deploy
    approval:
      description: "Promote build to production"
      groups: [release-approvers, security-leads]
      quorum: 2
    image: alpine
    script: ["echo deploying"]
```

Now the gate enforces:

- Only members of `release-approvers` OR `security-leads` can
  approve. Other users see the *Approve* button disabled.
- Two **distinct** members must approve. Same person clicking
  twice doesn't satisfy quorum.

`quorum: 1` is the default (single-approver, useful when
groups: alone is enough).

## Setting up groups

### Create the group

Admin → *Settings → Groups → New group*.

| Field | Value |
|---|---|
| Name | `release-approvers` |
| Description | Approvers for production releases. |

### Add members

*Settings → Groups → release-approvers → Add member*. Pick from the
user list — only authenticated users (already-onboarded) can be
added.

Group memberships are versioned: removing someone takes effect
immediately, but past approvals they cast remain valid (the audit
trail is immutable).

## Reject flow

Either approver can also click *Reject*. On reject:

- The job flips to `failed`.
- Subsequent stages are skipped.
- The run terminates as failed.
- A reject reason is captured (free-text comment from the
  rejecter, surfaced in the run detail page).

Reject is a hard stop — there's no "rejected pending re-approval"
state. To re-attempt, click *Run latest* on the pipeline.

## Notifications

Pair approval gates with the [notifications](/gocdnext/docs/pipelines/recipes/notifications/)
plugin so approvers get pinged when a gate is reached:

```yaml
notifications:
  - on: awaiting_approval
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ secrets.PROD_SLACK_WEBHOOK }}
      channel: "#prod-deploys"
      title: "🟡 Approval needed: ${CI_PROJECT_SLUG}"
      message: |
        *Pipeline*: ${CI_PIPELINE_NAME}
        *Branch*: ${CI_COMMIT_BRANCH}
        *Approve at*: ${CI_RUN_URL}
```

The `awaiting_approval` event fires the moment a job hits that
status. Don't pair this with `on: success` notifications for the
same channel — too chatty.

## Common patterns

### Promote across environments

```yaml
name: cd

stages: [build, staging, gate, prod]

jobs:
  build: { ... }

  deploy-staging:
    stage: staging
    needs: [build]
    image: ...
    script: ["./deploy.sh staging"]

  smoke-staging:
    stage: staging
    needs: [deploy-staging]
    image: ...
    script: ["./smoke.sh staging"]

  approve-prod:
    stage: gate
    needs: [smoke-staging]
    approval:
      description: "Smoke-tests passed on staging. Approve prod?"
      groups: [release-approvers]
      quorum: 1
    image: alpine
    script: ["echo approved"]

  deploy-prod:
    stage: prod
    needs: [approve-prod]
    image: ...
    script: ["./deploy.sh prod"]
```

Build → deploy staging → smoke → human → deploy prod.

### Multiple gates in one run

```yaml
jobs:
  approve-deploy:
    stage: deploy-gate
    approval:
      description: "Deploy?"
      groups: [release-approvers]
      quorum: 1

  approve-data-migration:
    stage: post-deploy-gate
    needs: [deploy]
    approval:
      description: "Run the data migration?"
      groups: [security-leads, dba]
      quorum: 2
```

Two gates, two distinct approver groups. Useful for high-stakes
operations where each step needs its own review.

### Auto-cancel after timeout

The `awaiting_approval` status can sit forever. To auto-cancel
after a window:

```yaml
approve-prod:
  stage: gate
  approval:
    description: "Approve prod"
    groups: [release-approvers]
    quorum: 1
  timeout: 24h
  ...
```

After 24h the job is killed (`failed` with a timeout reason),
the run terminates. Pair with a notification so approvers know
the window is closing.

## Audit trail

Every approve/reject click is captured in the `audit_events`
table:

```sql
SELECT actor_email, action, created_at, details
FROM audit_events
WHERE entity_type = 'job_run'
  AND action IN ('approval.approve', 'approval.reject')
ORDER BY created_at DESC
LIMIT 20;
```

The same entries are surfaced in *Settings → Audit log* with
filtering by user, project, action.

## Common pitfalls

- **Approver in the same group as the committer**: a developer
  approving their own PR's deploy. The platform doesn't enforce
  separation; if you need it, set group memberships exclusive
  (developers ≠ approvers).
- **`quorum > group size`**: setting quorum 3 but only 2 members
  in the group means the gate can never satisfy. Apply-time
  validation catches obvious cases (`quorum > len(union of
  members)` of all listed groups), but membership changes
  later can drop the count below the quorum mid-flight. Watch
  for stuck runs.
- **Approver without dashboard access**: the *Approve* button
  lives in the run detail page. Approvers need at least viewer
  role + login. If your approvers are external (a manager who
  never uses CI), that's a bigger flow than the gate alone
  solves.
- **Disabled accounts holding approvals**: if a user with
  approve permission was deactivated AFTER they approved, their
  approval is still valid (it landed at the time they were
  authorized). The audit trail records the historical state.
