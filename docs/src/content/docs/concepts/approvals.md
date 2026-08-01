---
title: Approval gates
description: Halt a run at a job until N members of an approver group sign off — basics, groups, quorum, and latest-wins supersede.
---

Approval gates pause a run at a specific job until human(s) click
*Approve* in the dashboard. Used for:

- Production deploys that need a second pair of eyes.
- Promotions across environments (staging → prod).
- Destructive operations (data migration, mass-update).
- Compliance flows (separation of duties).

A gate answers **who** may approve. It does not answer **whether anyone
should right now** — during a month-end close or an incident the gate is
still armed and still clickable. That is what an
[environment change-freeze](/gocdnext/docs/concepts/environment-freeze/)
is for: while the environment a gate governs is frozen, approving it is
refused with `409`. Rejecting stays available.

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

Each `approvers:` entry is matched against the deciding user's display
**name** or **email**, so either works (handy under OIDC, where the
identity is an email but the display name is a full name). For lists
that should survive a name/email change, prefer `approver_groups:` —
those match by user id.

`approvers:` and `approver_groups:` union — anyone in either list
counts toward the `required:` quorum.

## PR-label-driven quorum

Shipped in v0.13.0. When a run is triggered by a pull request, the
gate's effective quorum can be overridden based on labels carried
on the PR. Useful when one policy ("hotfix bypasses one of the two
approvers") shouldn't fork into a second pipeline file.

```yaml
jobs:
  promote-prod:
    stage: deploy
    approval:
      approver_groups: [release-approvers]
      required: 2            # base quorum (push, manual, tag…)
      quorum_by_label:
        hotfix: 1            # PR carrying `hotfix` → quorum 1
        breaking-change: 3   # PR carrying `breaking-change` → 3
      description: "Promote to prod"
```

**Semantics**:

- **PR cause only.** Push, manual, tag, upstream, schedule, poll —
  none of those carry labels, so the gate uses `required:`
  baseline.
- **Snapshot at run materialisation.** Labels read once from the
  PR webhook at run creation; relabel the PR afterward and the
  open gate keeps its frozen quorum (push a new head to
  re-materialise).
- **Multiple labels match → MAX wins.** A PR carrying both
  `hotfix` (override 1) and `breaking-change` (override 3) lands
  at quorum 3. Two reasons to demand more approvers don't cancel.
- **Ties broken lexicographically.** When two labels both override
  to the same value, the smallest-named label wins. Determinism
  matters for audit clarity.
- **No match keeps baseline.** PR with labels that don't intersect
  the map keeps `required:` unchanged; UI shows no override badge.

**UI signal**: when an override fires, the awaiting-approval card
gains a small `label <name>` badge next to the gate title. Hover
reveals "Quorum overridden to N by PR label X".

**Audit**: every override emits an `approval.quorum_overridden`
event with `{base_required, effective_required, label, cause}`
metadata. Default-quorum gates produce no audit row — the log only
records the policy events themselves.

**Validation** (parse-time, surfaces at `apply`, not runtime):

- Charset: alphanumeric + `.` `_` `-` `/`. GitHub case-insensitive
  labels lowercase automatically; `HotFix` in YAML and `hotfix` in
  the PR collapse to the same key.
- Override must be ≥ 1 (a quorum of 0 would auto-pass with no
  approver).
- Override must be ≤ `approvers + approver_groups` (un-passable
  detection same as base `required:`).
- Cap 16 entries per gate. Larger taxonomies belong in policy docs,
  not the pipeline YAML.
- Empty label keys + case-insensitive duplicate keys rejected.

**Provider coverage**: GitHub PRs only at v0.13.0. GitLab MR and
Bitbucket PR webhooks don't carry labels into gocdnext yet
([#11](https://github.com/klinux/gocdnext/issues/11),
[#12](https://github.com/klinux/gocdnext/issues/12)).

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
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
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

An abandoned gate has only two other exits: a human clicking, or
[supersede](#latest-wins-supersede) cancelling it when a newer run arrives.
Neither is guaranteed, so without a window a gate nobody returns to keeps its
run in `running` forever.

**Every gate has a window by default — no YAML required.** The server default
is **7 days**, set fleet-wide by the operator:

```bash
GOCDNEXT_APPROVAL_DEFAULT_TIMEOUT=168h   # default; "never" disables it
```

Override it per gate with the job-level `timeout:`:

```yaml
jobs:
  approve-prod:
    stage: gate
    approval:
      description: "Approve prod"
      approver_groups: [release-approvers]
      required: 1
    timeout: 24h     # this gate only; omit to inherit the server default
```

Precedence is `timeout:` → server default → never expire.

Accepted values are Go duration syntax between `1m` and `2160h` (90 days), or
`never`. Go's duration parser has **no day unit** — write `168h`, not `7d`; the
parser rejects the `d` suffix with a message naming the hours equivalent.

### Expired runs are canceled, not failed

An expired gate terminalises its run as **`canceled`** with
`cancel_reason = "approval timeout (168h) on gate \"approve-prod\""`, and the
gate itself records `decision = expired` (distinct from a rejection — a reject
is a decision, an expiry is the absence of one).

This is deliberate and load-bearing for your metrics: the dashboard computes
success rate as `success / (success + failed)` with canceled excluded, so
reporting abandonment as a failure would silently degrade every pipeline's
success rate. Nobody's build broke — nobody decided. DORA is unaffected either
way, since an expired gate never becomes a deploy.

Each expiry increments `gocdnext_approvals_expired_total` — a rising rate means
teams are opening gates they never come back to — and writes an
`approval.expired` audit event.

The audit write is best-effort: it happens after the cancel commits, so a
failure is logged rather than rolled back. Don't treat the audit log as a
complete ledger of expiries; the metric is the reliable count.

Cancelling also stops the run's still-executing jobs and tears down its
`services:` containers. Gates are armed from run creation, so a first-stage
build can still be running when a later gate times out.

:::note
The teardown is best-effort. If no Kubernetes agent is connected at the moment
the gate expires, `services:` pods can survive until a manual cleanup — the
server logs a warning naming the run. This only comes up with a window short
enough to expire while jobs are still running; under the 7-day default there is
nothing left running to leak.
:::

### Gates that must wait

Some gates legitimately sit for weeks — a compliance window, a release
scheduled for next month. Opt those out explicitly:

```yaml
    timeout: never
```

`never` beats the server default; nothing else does.

:::caution
The window is measured from when the gate started awaiting, which is **run
creation** — gates are armed for the whole run, not when their stage is
reached. A pipeline whose earlier stages take hours burns that time out of the
window. Harmless at the 7-day default; worth checking if you set a window
shorter than the pipeline's own duration.
:::

## Latest-wins supersede

Push three commits to a branch in a minute and you get three runs, all
parking at the same approval gate. Approving the *oldest* would then deploy
a **stale revision** over newer ones. `supersede:` fixes this: when a newer
run in the same lane becomes a pending contender at a gate, older pending
runs in that lane are canceled — so the pending pile normally clears to just
the newest. This pile-clear is best-effort (under lock contention it may skip
a victim and retry); the **hard guarantee that a stale deploy can never ship**
is the dispatch backstop described below.

Opt in per pipeline (off by default):

```yaml
name: api
supersede: branch   # off | branch | pipeline
stages: [build, approve-prod, deploy-prod]
```

- **`off`** (default) — every run waits independently; nothing is canceled.
- **`branch`** — the lane is `(pipeline, branch)`. Each feature branch is an
  independent lane; a new push to `main` only supersedes older `main` runs.
- **`pipeline`** — the lane is the whole pipeline (branch ignored). Use it
  when only one revision should ever be in flight regardless of branch.

Tag and manual runs (no branch) fall into a single per-pipeline lane.

### What actually gets superseded

Supersede is **environment-aware**. A gate's "environment" is resolved from
the deploy jobs it governs downstream — following stage order and explicit
`needs:` edges, stopping at the next gate on each path. So in
`build → approve-staging → deploy-staging → approve-prod → deploy-prod`,
`approve-staging` governs `staging` and `approve-prod` governs `prod`.

A newer run at the **staging** gate cancels older runs pending at staging,
but leaves alone an older run that already passed staging and is waiting at
**prod** — that contest is decided at the prod gate. A gate that governs no
deploy at all (a pure-approval pipeline) clears the whole pending pile.

### The hard guarantee

Even if the pile-clear races an approval, the dispatch path is the backstop:
a deploy is **refused** at dispatch if a newer, non-canceled run in the lane
(queued, running, or already deployed) has cleared the gate for that same
environment. This is fail-closed — on
any doubt the deploy is held, never shipped. Rollbacks are exempt (rolling
back to an older revision is an explicit, intended action).

A superseded run shows as **canceled** with a muted `superseded by #N` badge
linking to the run that won; the audit trail records `run.superseded` with
the counters (never a branch or ref value).

## Audit trail

Every approve/reject click is captured in the `audit_events`
table:

```sql
SELECT actor_email, action, created_at, details
FROM audit_events
WHERE entity_type = 'job_run'
  AND action IN ('approval.approve', 'approval.reject', 'approval.quorum_overridden')
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
- **Approve returns 409 and the message mentions a frozen
  environment**: the gate governs a deploy environment under a
  [change-freeze](/gocdnext/docs/concepts/environment-freeze/). It is
  not a permissions problem and not a stale tab — a maintainer has to
  lift the freeze. The error names every frozen environment that gate
  governs, so you don't discover them one retry at a time.
