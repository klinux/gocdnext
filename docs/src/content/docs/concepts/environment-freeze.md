---
title: Environment change-freeze
description: "Freeze a deploy environment and gocdnext admits no promotion to it — approving its gate, dispatching its deploy job, and rolling it back are all refused until a maintainer lifts the freeze."
---

An [approval gate](/gocdnext/docs/concepts/approvals/) answers **who**
may approve. It cannot answer **whether anyone should right now**.

During a month-end close, a holiday, or an incident, that gap is filled
socially: someone posts "nobody deploy to prod today" and hopes. The
gate is still armed, still clickable, and the person who approves at
23:40 was not in that channel.

A **change-freeze** makes it systemic. While `production` is frozen,
gocdnext admits **no** promotion to it:

| Action during a freeze | Result |
|---|---|
| Approving a gate that governs the frozen environment | **409** — the error names every frozen environment that gate governs |
| Dispatching a deploy job targeting it | Held: the job stays `queued`, the run shows `Frozen: production` |
| [Rolling back](/gocdnext/docs/concepts/deployments/#one-click-rollback) that environment | **409** |
| **Rejecting** a gate | **Allowed** — see below |

Rejecting is deliberately never refused. "Stop this from shipping" is
exactly the decision you still want available during a freeze.

## Freezing

Freeze from the **Environments** tab of a project, or via the API. A
**reason is required** — it is what every operator whose deploy is
refused will read, and a freeze nobody can explain at 03:00 is the
failure this feature exists to prevent.

```bash
# freeze
curl -X PUT /api/v1/projects/shop/environment-freezes/production \
  -H 'Content-Type: application/json' \
  -d '{"reason":"month-end close — no prod deploys until 2026-08-03"}'

# lift it
curl -X DELETE /api/v1/projects/shop/environment-freezes/production
```

| Method | Path | Effect |
|---|---|---|
| `PUT` | `/api/v1/projects/{slug}/environment-freezes/{name}` | freeze; body `{"reason": "..."}` (required, ≤500 chars) |
| `DELETE` | `/api/v1/projects/{slug}/environment-freezes/{name}` | lift the freeze |
| `GET` | `/api/v1/projects/{slug}/environments` | freeze state per environment |

Both writes are **maintainer+**. Both are **idempotent**: re-freezing an
already-frozen environment does *not* reset who froze it, when, or why —
the original record is what an audit asks about.

## The environment does not have to exist yet

This is the case worth understanding, because it is the one a freeze
most needs to cover.

Environments are **lazy**: the `production` environment is created the
first time a job actually deploys to it. So on a pipeline whose prod
stage has never run, there is no environment row to attach anything to
— and the first-ever deploy to production is precisely the one you
cannot afford to let through.

Freezes are therefore keyed by **`(project, environment name)`**, not by
an environment id. You can freeze a name that nothing has ever deployed
to, and it appears in the Environments tab as a card of its own with no
history and no version — just the freeze. The first deploy that would
have created that environment is held instead.

The same keying means a freeze **survives deleting the environment**.
Deleting a frozen environment leaves the freeze behind (still visible,
still liftable), so delete-and-recreate is not a way to launder one away.

## Two things that surprise people

**The name must match `deploy.environment` exactly — including case.**
The freeze is matched against the environment name in your pipeline
YAML, as a plain string. Freezing `production` does **not** hold a
pipeline that declares `environment: prod`, and `Production` is a
different environment from `production`. If a deploy you expected to be
held is running, check the name first.

**A freeze holds the deploy job, not the whole run.** A stage with a
frozen `production` deploy and a `staging` deploy that isn't frozen will
still ship staging. The run displays the freeze as its dominant queue
reason because that is the blocker a human has to act on — it does not
mean everything stopped.

## What the guarantee covers

The guarantee is at the **admission boundary**: once the freeze call
returns, no new deploy to that environment is *admitted*.

A deploy **admitted just before** the freeze committed still runs to
completion — the agent already has it, or the ArgoCD sync already went
out. Freezing does not reach across the network to stop work in flight,
and gocdnext does not pretend otherwise: cancelling an in-flight deploy
is a separate action you take deliberately.

In practice the window is the moment between a deploy being assigned and
the freeze landing. Everything queued behind it is held.

## Lifting a freeze

Lifting is one click, no reason required. Runs that were being held are
woken immediately rather than waiting for the next scheduler tick, so
deploys resume in about a second. There is no re-approval: a gate that
was already approved stays approved.

If a held run somehow isn't woken, the scheduler's periodic tick picks
it up on its next pass — the immediate wake is an optimisation over that
backstop, not a replacement for it.

## Who sees what

`GET /environments` is readable by any authenticated user, and the
response is role-sanitised:

| Field | Viewer | Maintainer / admin |
|---|---|---|
| `frozen`, `frozen_at` | ✅ | ✅ |
| `freeze_reason`, `frozen_by` | ❌ redacted | ✅ |

That split is deliberate. "Production is frozen" is operational state
everyone needs in order to understand why their pipeline is waiting. The
*reason* routinely names an incident, a customer, or an audit finding
("frozen: PCI finding INC-4412"), which is not viewer-grade information.

Write it for the maintainer who will read it, not as a broadcast.

## Audit

Freeze and unfreeze each write an `environment.freeze` /
`environment.unfreeze` row **in the same transaction as the change
itself**, so a freeze can never exist without a record of who set it and
why. The actor is derived server-side from the authenticated session —
never from anything the client sends.

Events are keyed `target_type=environment`,
`target_id=<project_id>:<name>`, which is what lets an investigation
filter to one project's freezes even for an environment that has never
been deployed to.

## What this is not

- **Not a pipeline pause.** The unit is a deploy *environment*, not a
  pipeline. Freezing `production` holds every pipeline in the project
  that deploys to it; it does not stop those pipelines from building
  and testing.
- **Not a cancel.** Work already admitted finishes. See
  [What the guarantee covers](#what-the-guarantee-covers).
- **Not scheduled.** Freezes are manual: someone sets it, someone lifts
  it. Calendar-driven freezes are not implemented.
- **Not org-wide.** A freeze is per project and per environment name.
  Freezing "everything" means freezing each one.
- **Not a control over external GitOps.** If ArgoCD auto-syncs your
  cluster from a Git branch, a freeze governs what *gocdnext* admits —
  it cannot stop a sync it doesn't drive.

## See also

- [Approval gates](/gocdnext/docs/concepts/approvals/) — *who* may
  approve, the complement to *whether anyone should now*
- [Deployments & rollback](/gocdnext/docs/concepts/deployments/) —
  environments, revisions, and the rollback a freeze refuses
- [Native deploys (ArgoCD)](/gocdnext/docs/concepts/native-deploy/) —
  server-managed deploys, held by a freeze the same way agent deploys are
