---
title: Deployments & rollback
description: "The deploy: marker tracks which version shipped to which environment — a tracking layer over your real deploy job, with one-click rollback that re-runs the original."
---

The `deploy:` block marks an executable job as a deployment to a
named environment. The job still runs your real deploy `script:` /
`uses:` — gocdnext does **not** ship anything for you. `deploy:` is a
**tracking marker**: when the job succeeds, gocdnext records that
this run shipped `version` to `environment`. That record powers the
Environments tab — current version per environment, full history,
and one-click rollback.

By default this is a tracking layer, not an executor. gocdnext
doesn't invent a deploy mechanism (no built-in `kubectl apply`, no
implicit Helm) — your job already knows how to deploy. The primitive
adds the *visibility* a CD tool owes you: what's live where, when it
changed, and who can roll it back.

The **exception is opt-in**: register a
[native deploy target](/gocdnext/docs/concepts/native-deploy/) for an
environment and gocdnext *does* execute the deploy for it — driving an
ArgoCD sync and watching the Application to `Synced + Healthy`,
server-side with no agent. The `deploy:` block below is identical
either way; the environment having a registered target is what turns
it native. Everything else on this page describes the tracking-layer
default.

## Declaring a deployment

```yaml
jobs:
  ship-prod:
    stage: deploy
    image: google/cloud-sdk:slim
    deploy:
      environment: production
      version: ${{ needs.build.outputs.image-tag }}
    script:
      - ./deploy.sh
```

| Key | Type | Notes |
|---|---|---|
| `environment` | string (**required**) | Target environment name. Lazy-created on first deploy — no pre-registration, no separate "create environment" step. |
| `version` | string (optional) | The version string recorded as deployed. References allowed: `${{ needs.X.outputs.Y }}`, `${{ CI_* }}`, `${CI_*}`. Resolved against **CI vars only — never secrets** (the version is persisted and shown in the UI). Omitted → defaults to `CI_COMMIT_SHORT_SHA`. |

A deploy job is a normal executable job, so it carries everything a
job carries — `needs:`, `artifacts:`, `id_tokens:`, `secrets:`. The
parser rejects `deploy:` on an [approval gate](/gocdnext/docs/concepts/approvals/)
(a gate blocks on a human decision; it doesn't deploy). The common
shape is therefore two jobs — a gate, then the deploy:

```yaml
jobs:
  promote-prod:
    stage: deploy
    approval:
      description: "Promote build to production"
      approver_groups: [release-approvers]
  ship-prod:
    stage: deploy
    needs: [promote-prod]
    deploy:
      environment: production
      version: ${{ needs.build.outputs.image-tag }}
    script: [./deploy.sh]
```

## The version comes from outputs, not a snapshot

`version` is resolved at dispatch like any other field, but against
CI vars only. The idiomatic source is an upstream job's
[output](/gocdnext/docs/pipelines/yaml-reference/#job-outputs-outputs):
a `build` job writes the image tag it produced to
`$GOCDNEXT_OUTPUT_FILE`, the deploy job reads it back as
`${{ needs.build.outputs.image-tag }}`.

If `version` can't resolve — a `${{ CI_TAG_NAME }}` on a non-tag run,
or an omitted version on a run with no commit short sha — the deploy
job fails **terminally** at dispatch (it does not retry forever
against an identical failure). The error names the missing reference,
never the value of anything next to it.

## Environments and revisions

An **environment** is just a `(project, name)` pair, created the
first time a job deploys to it. It holds no config — it's a label the
history hangs off.

Every dispatch of a deploy job writes a **deployment revision**:

- created `in_progress` between assignment and dispatch,
- finalized to `success` or `failed` by the job's terminal result —
  **a deploy's outcome IS its job's outcome**, no separate health
  check,
- keyed by `(job_run_id, attempt)` so a [single-job rerun](/gocdnext/docs/pipelines/yaml-reference/)
  or retry records a distinct revision rather than mutating the
  prior one.

The Environments tab shows the current (latest `success`) version
per environment and the full revision history. Via the API:

| Method | Path | Returns |
|---|---|---|
| `GET` | `/api/v1/projects/{slug}/environments` | environments with their current version |
| `GET` | `/api/v1/projects/{slug}/environments/{envID}/deployments` | revision history for one environment |
| `POST` | `/api/v1/projects/{slug}/environments/{envID}/rollback` | roll back (see below) |

## One-click rollback

Each successful deploy in an environment's history offers a roll-back
action — **as long as its run still exists**. Rollback does not snapshot
manifests or override a version. It **re-runs the deploy job of the
chosen past run**.

That works because of one invariant: a finished run's job outputs are
**immutable**. Re-running that run's deploy job re-resolves
`${{ needs.*.outputs.* }}` to the *same* values it saw originally, so
`./deploy.sh` ships the *same* version again. The old version
"freezes" for free — no version field to carry, no manifest to store.
The new revision is flagged `is_rollback = true` so the history
distinguishes it from a forward deploy.

Because rollback replays a real job, it goes through the same
dispatch path — same agent, same secrets, same `id_tokens:`. If the
source run has since been pruned (its outputs are gone), the roll-back
action isn't offered: there's nothing immutable left to replay.

## What this is not

- **Not an executor** — *unless* the environment has a
  [native deploy target](/gocdnext/docs/concepts/native-deploy/). By
  default there's no environment-level config, no built-in apply step,
  no drift detection; your `script:` owns the mechanism. A native
  target is the deliberate opt-in that makes gocdnext drive the deploy.
- **Not a gate.** Gating a deploy is the job of an
  [approval](/gocdnext/docs/concepts/approvals/) on a *separate* job
  upstream of the deploy.
- **Not a health monitor.** Success tracks the deploy *job*. If your
  rollout needs a post-deploy smoke test, make it part of the
  `script:` (or a downstream job) so its failure fails the deploy.

## See also

- [`deploy:` in the YAML reference](/gocdnext/docs/pipelines/yaml-reference/#deployments-deploy)
- [Job outputs](/gocdnext/docs/pipelines/yaml-reference/#job-outputs-outputs) — where `version` usually comes from
- [Approval gates](/gocdnext/docs/concepts/approvals/) — the gate half of promote → deploy
