---
title: Pipeline quickstart
description: Smallest end-to-end pipeline that exercises every layer.
---

This walks the smallest pipeline that does something useful: build
a Go binary, run its tests, ship logs to the dashboard. Replace `go`
with whatever your project uses; the shape is identical.

## 1. Drop a YAML in `.gocdnext/`

In the repo you want gocdnext to build:

```yaml title=".gocdnext/ci.yaml"
name: ci
when:
  event: [push, pull_request]

stages: [test, build]

jobs:
  unit:
    stage: test
    uses: gocdnext/go@v1
    with:
      command: test -race ./...

  compile:
    stage: build
    uses: gocdnext/go@v1
    needs: [unit]
    with:
      command: build -o bin/app ./cmd/app
    artifacts:
      paths: [bin/app]
```

What this declares, in order:

- `name`: pipeline identifier (must be unique per project).
- `when.event`: webhooks of these types create a run; everything else
  is ignored.
- `stages`: ordered list. Jobs in stage *N+1* don't dispatch until
  every job in stage *N* succeeds.
- `jobs.unit / compile`: each job is one container. `uses:` picks a
  catalog plugin (here `gocdnext/go`); `with:` is the input map the
  plugin's manifest validates.
- `needs:` lets you order jobs *inside* the same stage; cross-stage
  ordering is implicit.
- `artifacts.paths`: files copied off the worker after the job, into
  the artefact backend, addressable by URL in the run detail page.

## 2. Apply

Pick one of:

**One-shot** (tying a checkout you already have):

```bash
gocdnext apply --slug myapp --name "My app" .
```

**Webhook-driven** (the production path): connect a GitHub repo via
*Settings → SCM* in the dashboard. From then on every push that
matches `when.event` creates a run automatically.

## 3. Watch the run

Go to *Projects → My app*. You'll see:

- The pipeline tile with a status dot, the latest run number, and a
  *bottleneck* hint (slowest stage in the historical median, when
  enough runs exist).
- Click the tile → run detail. Three tabs:
  - **Jobs**: per-stage breakdown with logs streamed live via SSE.
  - **Tests**: JUnit output if any job uploaded a `test_reports:`
    artefact.
  - **Artifacts**: the files `artifacts.paths` shipped, signed-URL
    download.

## 4. What changed under the hood

When the webhook fired:

1. Server validated the HMAC, extracted `(repo, branch, sha)`.
2. Matched it against the project's `scm_source` and inserted a
   `modification` row.
3. `CreateRunFromModification` inserted `run` + `stage_runs` + `job_runs`
   atomically and `pg_notify('run_queued', <run_id>)`.
4. The scheduler woke up via `LISTEN run_queued`, picked the agent
   with matching tags + free capacity, sent a `JobAssignment` over
   the gRPC stream.
5. The agent cloned the material, spawned the plugin container,
   streamed log lines back. Server batched lines (100 lines / 200 ms)
   into Postgres and fan-published them to SSE subscribers in the
   dashboard.
6. On terminal status, server promoted the stage, dispatched the
   next, archived the logs (when configured), and reported back to
   GitHub Checks.

## Where to next

- [Recipes](/gocdnext/docs/pipelines/recipes/go-monorepo/) for richer
  pipelines (Go monorepo, SSH deploy, Helm release).
- [Plugin catalog](/gocdnext/docs/reference/plugins/) for the full
  list of `uses:` references and their `with:` schemas.
- [Helm install](/gocdnext/docs/install/helm/) if you got here by
  reading the dashboard of someone else's deployment and now want
  your own.
