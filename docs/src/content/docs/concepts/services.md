---
title: Services lifecycle
description: Declare sidecar containers (databases, queues, mocks) alongside a run — what they look like, when they're ready, how the UI surfaces them.
---

A **service** is a sidecar container the platform brings up
alongside a run so the run's jobs have something to talk to:
Postgres for integration tests, Redis for queues, LocalStack for
AWS APIs, a mock HTTP server, anything that needs a network
endpoint.

Services are declared at the pipeline level. Every service comes
up at the start of the run and is reachable by every job in that
run — there is no per-job opt-in. See [YAML reference → Services](/gocdnext/docs/pipelines/yaml-reference/#services)
for the syntax.

## What gets created

For each `services:` entry that's referenced by at least one job in
a run, the agent creates:

- **Docker engine**: a container on the run's bridge network, with
  the service's `name:` as DNS alias.
- **Kubernetes engine**: a Pod in the run's namespace with a `Service`
  resource fronting it, alias = `name`.

The job's environment gets the alias as a hostname — `psql -h postgres`
works from anywhere in the job script.

## Lifecycle states

| State | When |
|---|---|
| `starting` | Container/Pod created; waiting for ready signal |
| `ready` | Docker: container `running` + healthy. K8s: `Pod.Status.Phase == Running` |
| `failed` | Crashed before ready (image pull error, exit ≠ 0 in startup, OOMKilled). Job fails fast with the service's status. |
| `stopped` | Cleanly torn down at run end. This is the happy-path terminal state. |

Status transitions emit a `ServiceLifecycle` event over the agent's
gRPC stream; the server upserts into the `service_runs` table with a
sticky-failed guard (a once-`failed` service can't be reset back to
`ready` if the container is restarted by the runtime).

## How the UI surfaces them

### Run detail

In the run detail page, services appear as a **Setup column** before
the first stage of the pipeline canvas. Each service is a circle
coloured by status (running = blue, ready = green, failed = red,
stopped = green). Click to open a popover with image, current
status, started/finished timestamps, and an inline tail of the
service's logs.

The Setup column folds into the same `success/running/failed`
aggregate as a normal stage, so the run's overall status reflects
whether the services are healthy:

- Any `failed` → run header turns red (`failed`).
- All `ready` or `stopped` → counts as `success` (doesn't dim the
  next stage).
- Any `starting` → `running` (animated spinner).

### Project page pipeline cards

Each card on `/projects/<slug>` shows the latest run's pipeline
strip. When that run has services, they show as small circles at
the start of the strip with the same colours as the run detail
canvas. Hover gives `service-name: status`.

The list endpoint serialises a `has_services: bool` flag per run
summary so the card only fetches the service detail when the run
actually has them — no extra request for service-free pipelines.

## Failure semantics

- **Service fails before any job starts** → the run is marked
  `failed` immediately; no job runs.
- **Service fails mid-job** → the job continues until it exits on
  its own (you don't lose stdout). The aggregated run status is
  still `failed` because the service is.
- **Service is OK at start, dies mid-job** → no lifecycle change.
  The job sees connection errors and likely fails. The dashboard
  shows the service as `ready` until it's reaped; the job's logs
  are where the failure surfaces.

Services are not auto-restarted. The model is "the service must
come up clean once; if it dies, the job dies with it." This keeps
the failure attribution clear.

## Limits

- One sidecar per service entry. To run e.g. a Postgres + a Redis,
  declare both at the pipeline level and list both in the job's
  `services:` array.
- Services don't see the workspace volume — they're standalone
  containers with their own root filesystem. Pass configuration via
  `env:` or `command:`.
- No health-check `wait_for`-style block yet. If a service needs
  N seconds to be query-able even after `ready`, the job script
  is responsible for a small `wait-for-it.sh`-style loop.

## See also

- [Kubernetes runtime](/gocdnext/docs/concepts/kubernetes-runtime/) —
  how services are wired up in the isolated workspace model.
- [YAML reference → Services](/gocdnext/docs/pipelines/yaml-reference/#services) —
  full syntax surface.
