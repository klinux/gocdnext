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
```

When the run reaches this job, its status flips to
`awaiting_approval`. The dashboard surfaces a banner with
*Approve / Reject* buttons. The first authenticated user with
the role to approve clicks → the gate passes → the run continues
to the next stage.

Approval jobs are **gates, not executors** — the parser rejects
mixing `approval:` with `image:`, `uses:`, `script:`, or
`artifacts:` on the same job. The gate doesn't run a command;
clicking *Approve* IS the action.

Without `approvers:` or `approver_groups:` set, ANY authenticated
user (admin, maintainer, or viewer) can approve. The audit trail
records who clicked.

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
      approver_groups: [release-approvers, security-leads]
      required: 2
```

Now the gate enforces:

- Only members of `release-approvers` OR `security-leads` can
  approve. Other users see the *Approve* button disabled.
- Two **distinct** members must approve. Same person clicking
  twice doesn't satisfy quorum.

`required: 1` is the default (single-approver, useful when
`approver_groups:` alone is enough).

The YAML keys are `approver_groups:` and `required:` (the parser's
canonical names). The dashboard surfaces them as "Groups" and
"Quorum" in the approval modal — both spellings refer to the same
field.

You can also pin individual approvers without a group:

```yaml
approval:
  description: "Sign-off needed"
  approvers: [alice@example.com, bob@example.com]
  required: 1
```

`approvers:` and `approver_groups:` union — anyone in either list
counts toward the `required:` quorum.

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

Pair approval gates with a notification so approvers get pinged
when a gate is reached. Notification triggers (`on:`) accept
`failure`, `success`, `always`, `canceled` — there's no
`awaiting_approval` trigger today. Use a no-op job placed right
before the gate, or hook a webhook from outside:

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ PROD_SLACK_WEBHOOK }}
      channel: "#prod-deploys"
      template: |
        :x: ${CI_PIPELINE_NAME} (${CI_COMMIT_BRANCH}) failed
        ${CI_RUN_URL}
    secrets: [PROD_SLACK_WEBHOOK]
```

If a gate-arrived notification matters, watch the issue tracker
for the upcoming `on: awaiting_approval` trigger.

## Common patterns

### Promote across environments

```yaml
name: cd

stages: [build, staging, gate, prod]

jobs:
  build:
    stage: build
    image: alpine
    script: ["./build.sh"]

  deploy-staging:
    stage: staging
    needs: [build]
    image: alpine
    script: ["./deploy.sh staging"]

  smoke-staging:
    stage: staging
    needs: [deploy-staging]
    image: alpine
    script: ["./smoke.sh staging"]

  approve-prod:
    stage: gate
    needs: [smoke-staging]
    approval:
      description: "Smoke-tests passed on staging. Approve prod?"
      approver_groups: [release-approvers]
      required: 1

  deploy-prod:
    stage: prod
    needs: [approve-prod]
    image: alpine
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
      approver_groups: [release-approvers]
      required: 1

  approve-data-migration:
    stage: post-deploy-gate
    needs: [approve-deploy]
    approval:
      description: "Run the data migration?"
      approver_groups: [security-leads, dba]
      required: 2
```

Two gates, two distinct approver groups. Useful for high-stakes
operations where each step needs its own review.

### Auto-cancel after timeout

The `awaiting_approval` status can sit forever. To auto-cancel
after a window:

```yaml
jobs:
  approve-prod:
    stage: gate
    approval:
      description: "Approve prod"
      approver_groups: [release-approvers]
      required: 1
    timeout: 24h
```

After 24h the job is killed (`failed` with a timeout reason); the
run terminates.

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
- **`required > group size`**: setting `required: 3` but only 2
  members in the listed groups means the gate can never satisfy.
  Apply-time validation catches obvious cases, but membership
  changes later can drop the count below quorum mid-flight. Watch
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
