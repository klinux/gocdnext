---
title: Services lifecycle
description: Declare sibling service containers (databases, queues, mocks) alongside a run — what they look like, when they're ready, how the UI surfaces them.
---

A **service** is a sibling container or Pod the platform brings
up alongside a run so the run's jobs have something to talk to:
Postgres for integration tests, Redis for queues, LocalStack for
AWS APIs, a mock HTTP server, anything that needs a network
endpoint.

Services are **not sidecars** — they don't live in the same Pod /
container as the job. They're separate workloads on a shared
network with DNS-name discovery. Closest mental model is the
`services:` block in GitHub Actions and GitLab CI, or
docker-compose without the YAML overhead.

Services are declared at the pipeline level. Every service comes
up at the start of the run and is reachable by every job in that
run — there is no per-job opt-in. See [YAML reference → Services](/gocdnext/docs/pipelines/yaml-reference/#services)
for the syntax.

## What gets created

For each `services:` entry that's referenced by at least one job in
a run, the agent creates a separate workload and wires DNS:

- **Docker engine**: a **standalone container** on a job-scoped
  bridge network (`gocdnext-<jobShort>`). The container is started
  with `--network-alias <svc-name>`, so the task container — which
  joins the same network — resolves `postgres:5432` via docker's
  embedded DNS. Per-job today; the run-scoped reuse model is k8s-
  only.
- **Kubernetes engine**: a **separate Pod** in the agent's
  namespace, named `gocdnext-svc-<runShort>-<svc-name>`. The job
  Pod is created with `spec.hostAliases: [{ip: <svc-pod-IP>,
  hostnames: [<svc-name>]}]` so `getent hosts postgres` inside the
  task container returns the service Pod's IP. **No Kubernetes
  `Service` resource is created** — `kubectl get svc` won't show
  anything. Service Pods are run-scoped and shared by every job
  of the run (the first job creates them; siblings adopt by name +
  label match).

The job's environment ends up with the alias as a hostname —
`psql -h postgres` works from anywhere in the job script — but
the wiring is different per engine. Don't expect a `kubectl get
svc` workflow to surface the Postgres endpoint; check `kubectl
get pods -l app.kubernetes.io/component=service,gocdnext.io/run-id=<id>`
instead.

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

- One container/Pod per service entry. To run e.g. a Postgres + a
  Redis, declare both at the pipeline level and list both in the
  job's `services:` array.
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
