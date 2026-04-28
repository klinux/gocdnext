---
title: YAML reference
description: Every key the pipeline parser accepts, what it means, and where to use it.
---

`.gocdnext/<name>.yaml` is parsed by the server's pipeline parser.
This page catalogs every accepted key with its shape, default,
and a short example. Keys not on this list are rejected at apply
time — typos surface as errors instead of being silently ignored.

## Top-level

```yaml
name: ci                    # string, required, unique per project
description: "..."          # string, optional, surfaced in the UI

when:                       # trigger gates; see "Triggers" below
  event: [push, pull_request]
  branches: [main]
  paths: ["src/**"]

stages: [lint, test, build] # ordered list; each stage waits for the
                            # previous to fully succeed

materials:                  # extra checkouts beyond the implicit one;
  - upstream:               # see "Materials" below
      pipeline: ci-server
      stage: test
      status: success

services:                   # sidecar containers, run alongside every
  - name: postgres          # job; see "Services" below
    image: postgres:16

variables:                  # env vars merged into every job
  GOCACHE: /workspace/.go-cache

notifications:              # post-run hooks; see "Notifications"
  - on: failure
    uses: gocdnext/slack@v1
    with: { ... }

jobs:                       # map; keys are job names
  unit:
    ...
```

## Triggers (`when:`)

Controls which webhooks materialise into runs.

| Key | Type | Default | Notes |
|---|---|---|---|
| `event` | `[]string` | `[push]` | `push`, `pull_request`, `tag`, `manual`, `cron`, `upstream` |
| `branches` | `[]string` | all | match by exact name or `glob:*` |
| `paths` | `[]string` | all | glob — run only when the push touches a matching path |
| `tag_name` | string | — | regex applied to tag name (`event: [tag]`) |

```yaml
when:
  event: [push, pull_request]
  branches: [main, "release/**"]
  paths: ["server/**", "go.work"]
```

`event: [manual]` makes the pipeline runnable only via the *Run latest*
button or `gocdnext run <pipeline>`. `event: [cron]` is set
automatically when the project has a cron schedule pointing at
this pipeline.

## Materials

Extra checkouts beyond the implicit project material. Each entry
is one of `git`, `upstream`, `cron`, or `manual`.

### `git` — additional repository

```yaml
materials:
  - git:
      url: https://github.com/org/shared-libs
      branch: main
      path: ./vendor/shared-libs
      poll_interval: 5m       # optional polling fallback
```

Cloned into `path:` relative to the workspace root. Useful for
downstream jobs that need a sibling repo's source.

### `upstream` — depend on another pipeline

```yaml
materials:
  - upstream:
      pipeline: ci-server
      stage: test
      status: success
```

When `ci-server.test` finishes successfully, the platform creates
a downstream run **with the same revision** in the same project.
This is gocdnext's fanout primitive — it's how monorepos chain
pipelines. The downstream's `cause` is `upstream`; the dashboard
shows a banner linking back to the trigger run.

### `cron` — scheduled trigger

Cron schedules are configured per-project in *Settings → Crons*,
not in YAML. The parser side is automatic — when a cron fires
that points at this pipeline, the run's `cause` is `cron`.

### `manual` — only via UI/CLI

Equivalent to `when.event: [manual]`. Runs created via the
*Run latest* button or `gocdnext run`.

## Stages

```yaml
stages: [lint, test, build, deploy]
```

Ordered list. Stage *N+1* dispatches when every job in stage *N*
hits a terminal status:
- All `success` → next stage runs.
- Any `failed` → run is marked failed; stages past N are skipped.
- Any `awaiting_approval` → run holds at the boundary until
  approved (see Approval gates).

## Jobs

```yaml
jobs:
  <name>:
    stage: <stage-name>      # required, must be in `stages:`
    image: alpine:3.20       # OR `uses:` (mutually exclusive)
    uses: gocdnext/go@v1
    with: { command: test ./... }
    needs: [other-job]       # ordering inside the same stage
    needs_artifacts:
      - from_job: deps
        paths: [node_modules/]
    docker: true             # mounts host docker.sock
    services: [postgres]     # references service name; see below
    variables:
      MY_VAR: hello
    secrets: [SLACK_TOKEN]
    cache:
      - key: ...
        paths: [...]
    artifacts:
      paths: [...]
      optional:
        paths: [...]
    test_reports:
      paths: ["**/*.xml"]
    matrix:
      python_version: ["3.11", "3.12"]
    approval:
      groups: [release-approvers]
      quorum: 2
    timeout: 30m
    agent:
      profile: gpu
      tags: [linux, x86_64]
    when:                    # per-job gate, on top of pipeline-level
      event: [tag]
```

Per-key:

| Key | Type | Notes |
|---|---|---|
| `stage` | string | required |
| `image` | string | container image; `script:` runs in `sh -c` |
| `script` | `[]string` | shell lines; mutually exclusive with `uses:` |
| `uses` | string | plugin reference: `gocdnext/<name>@v1` or `ghcr.io/...@v1` |
| `with` | map | inputs passed as `PLUGIN_*` env to the image |
| `needs` | `[]string` | other jobs in the same stage that must finish first |
| `needs_artifacts` | list | tar of upstream job's artefacts restored before run |
| `docker` | bool | mount host docker.sock — for testcontainers, buildx, etc. |
| `services` | `[]string` | sidecar containers (declared at pipeline level) |
| `variables` | map | env vars merged into the job's environment |
| `secrets` | `[]string` | project/global secrets injected as env, masked in logs |
| `cache` | list | tar paths between runs, keyed by template string |
| `artifacts` | list | files to ship to the artefact backend (paths, optional, retention) |
| `test_reports` | list | JUnit XML globs surfaced in the Tests tab |
| `matrix` | map | expand the job into one cell per cartesian product |
| `approval` | block | gate on human approval (groups + quorum) |
| `timeout` | duration | hard kill after — `30m`, `2h` |
| `agent` | block | runner profile + tags |
| `when` | block | per-job trigger gate (intersected with pipeline-level) |

## Cache

```yaml
cache:
  - key: pnpm-store-${CI_COMMIT_BRANCH}
    paths: [.pnpm-store, node_modules]
```

`key` is a template — variables expanded:

| Variable | Expands to |
|---|---|
| `${CI_COMMIT_BRANCH}` | branch name (sanitised) |
| `${CI_PIPELINE_NAME}` | pipeline name |
| `${CI_PROJECT_SLUG}` | project slug |

Different branches with the same key share the cache; same branch
with different keys keeps separate buckets. A typical pattern is
`<tool>-${CI_COMMIT_BRANCH}` so main and feature branches don't
poison each other.

## Artifacts

```yaml
artifacts:
  paths:
    - dist/myapp
    - "build/reports/**/*.html"
  retention: 30d           # optional; falls back to global default
  optional:
    paths:
      - "**/coverage.xml"  # only published if present
```

`paths:` is required — the job fails if a listed path doesn't exist.
`optional.paths:` is "publish if present, no-op if missing" —
useful for coverage reports that conditional jobs may or may not
generate.

## Test reports

```yaml
test_reports:
  paths: ["**/junit.xml"]
```

JUnit XML files matched by the glob are parsed at job completion
and populate the run's *Tests* tab — per-case status, duration,
failure message + stack trace.

## Matrix

```yaml
jobs:
  test:
    matrix:
      go_version: ["1.23", "1.24", "1.25"]
      os: [ubuntu, alpine]
    image: golang:${{ matrix.go_version }}-${{ matrix.os }}
    script:
      - go test ./...
```

Cartesian expansion — 3 × 2 = 6 jobs. `${{ matrix.X }}` is
substituted in any string field. Failure of one cell does NOT
stop sibling cells; the run aggregates `success` only when every
cell succeeds.

## Services

```yaml
services:
  - name: postgres
    image: postgres:16
    command: ["-c", "fsync=off"]
    env:
      POSTGRES_PASSWORD: test

jobs:
  integration:
    stage: test
    image: golang:1.25-alpine
    services: [postgres]    # by name
    script:
      - psql -h postgres -U postgres -c 'select 1'
```

Each declared service spins up alongside every job that lists it
in `services:` (by name). The service's `name:` becomes the DNS
alias inside the job's network. Services share the run; they're
torn down when the run finishes.

## Secrets

```yaml
jobs:
  deploy:
    secrets: [SSH_DEPLOY_KEY, SSH_KNOWN_HOSTS]
    uses: gocdnext/ssh@v1
    with:
      key: ${{ secrets.SSH_DEPLOY_KEY }}
      known_hosts: ${{ secrets.SSH_KNOWN_HOSTS }}
```

Secrets are AES-256-GCM-encrypted at rest. Listed names are
resolved at dispatch time from the project's secret store
(*Project → Secrets*), injected as env vars, and masked in
streamed log lines. Global secrets fall through when a project
secret with the same name doesn't exist.

## Approval gates

```yaml
jobs:
  promote-prod:
    stage: deploy
    approval:
      description: "Promote build to production"
      groups: [release-approvers]
      quorum: 2
    image: alpine
    script: ["echo promoting"]
```

Job sits at `awaiting_approval` until the configured number of
distinct members of the listed groups click *Approve* in the
dashboard. Quorum 1 = single-approver gate; 2+ enforces multi-
party review.

## Notifications

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ secrets.SLACK_WEBHOOK }}
      channel: "#ci-alerts"
  - on: success
    uses: gocdnext/discord@v1
    with: { ... }
```

`on:` accepts `success`, `failure`, `cancelled`, `always`. The
notification runs as a synthetic job after the main run terminates;
its log appears in the run detail page like any other job.

## Per-job `when:`

```yaml
jobs:
  deploy:
    when:
      event: [push]
      branches: [main]
    ...
```

Intersected with the pipeline-level `when:`. Common pattern: most
jobs run on every push, deploy job runs only on main.

## Agent selection

```yaml
agent:
  tags: [linux, gpu]
  profile: gpu-pool
```

`tags:` filters which agents can claim the job (set on the agent's
`GOCDNEXT_AGENT_TAGS`). `profile:` references a runner profile
(see *Settings → Runner profiles*) — admins define what infra each
profile means (k8s nodeSelector, resources, etc.); pipelines pick
by name.

## Timeout

```yaml
jobs:
  long-thing:
    timeout: 2h
```

Hard kill if the job hasn't reached terminal status within the
window. Default is no timeout (jobs can run indefinitely). Use
this to protect against wedged tests or infinite loops.
