# Pipeline spec (`.gocdnext/*.yaml`)

Schema reference. See [examples/](../examples/) for working pipelines.

## File layout

Every repo has a **`.gocdnext/` folder** at its root. The server loads every
`*.yaml` and `*.yml` file inside it. **One file = one pipeline.**

```
my-repo/
├── .gocdnext/
│   ├── build.yaml         → pipeline "build" (name from filename)
│   ├── tests.yaml         → pipeline "tests"
│   └── deploy-prod.yaml   → pipeline "deploy-prod"
└── src/…
```

Pipeline name resolution:
1. `name:` field inside the file (preferred — explicit).
2. Filename without extension (fallback).

Two files with the same resolved name is an error.

## Top-level

```yaml
version: "1"                    # optional, reserved
name: my-pipeline               # optional; overrides filename
include: [...]                  # optional: local/remote/template YAMLs
materials: [...]                # required: what triggers this pipeline
stages: [...]                   # required: ordered list of stage names
variables: {...}                # optional: global env vars
template: name                  # optional: inherit from a pipeline template
jobs: {...}                     # required: map of job-name → JobDef
```

## Materials

One material per list entry. Exactly one of `git`, `upstream`, `cron`, `manual`.

```yaml
materials:
  - git:
      url: https://github.com/org/repo
      branch: main               # default "main"; supports glob for feature branches
      on: [push, pull_request]   # default [push]
      auto_register_webhook: true
      poll_interval: 5m          # optional; server polls HEAD every N when webhooks can't reach us
      secret_ref: github-app-org # k8s secret or secret-manager ref
  - upstream:
      pipeline: build-core
      stage: test
      status: success             # default "success"
  - cron:
      expression: "0 */2 * * *"
  - manual: true
```

### Polling vs. webhook

Webhooks are the default — `auto_register_webhook: true` makes the server
install a hook on the repo and fire a run on every push. When the repo can't
reach us (corporate firewall, self-hosted Git behind VPN), set
`poll_interval:` and the server checks the branch HEAD every N duration
instead. Both can coexist: the server deduplicates via the modification's
unique `(material_id, revision, branch)` key, so a webhook arrival between
polls doesn't create a duplicate run.

`poll_interval` accepts Go duration strings (`1m`, `5m`, `1h30m`, `2h`) with
bounds `[1m, 24h]`. Empty / unset disables polling for this material.

Project-level fallback: operators can also set polling at the project level
from `/projects/{slug}/settings` — that interval applies to the synthesized
implicit project material (the repo bound via scm_source). Per-material
`poll_interval:` in YAML wins over the project default when both are set.

### Project-level schedules

In addition to per-pipeline `cron:` materials, operators can create
project-wide schedules from `/projects/{slug}/crons`. Each schedule fires
either every pipeline in the project (the default) or a pinned subset. Runs
created this way are tagged `cause=schedule` and attributed to
`cron:<schedule_name>`. Use them when the same cron expression would
otherwise have to be pasted into every pipeline's YAML.

Manual counterpart: the project header's **Run all pipelines** action uses
the same fire mechanism (just synchronous + tagged `cause=manual`), so a
cron-fired nightly build and an operator-clicked "run everything now"
produce observationally identical runs apart from the cause column.

## Jobs

```yaml
jobs:
  <name>:
    stage: <stage-name>           # must be in top-level stages list
    image: <container-image>      # job runtime OR plugin image
    needs: [job1, job2]           # intra-pipeline DAG; fast-fail
    script:                       # shell tasks (run sequentially in `image`)
      - make build
      - make test
    settings:                     # plugin settings (→ PLUGIN_* env vars)
      channel: "#deploys"
    variables:
      FOO: bar
    cache:
      paths: [~/.cache/go-build]
    artifacts:
      paths: [bin/]
      expire_in: 7d
    parallel:
      matrix:
        - OS: [linux, darwin]
          ARCH: [amd64, arm64]
    rules:
      - if: $CI_COMMIT_BRANCH == "main"
      - if: $CI_PIPELINE_SOURCE == "pull_request"
        when: manual
    when:
      status: [success, failure]
      branch: [main, release/*]
    timeout: 30m
    retry: 2
```

### Approval gates

A job can block the pipeline on a human decision by setting `approval:`
instead of `script:`/`uses:`. The gate parks in `awaiting_approval` until
someone votes through the web UI or the `POST /api/v1/job_runs/{id}/approve`
endpoint.

```yaml
jobs:
  approve-prod:
    stage: deploy
    approval:
      approvers: [alice, bob]          # individual users (name OR email)
      approver_groups: [sre, leads]    # groups — any member can approve
      required: 2                      # quorum — default 1
      description: "Deploy to prod?"
```

- **`approvers:`** and **`approver_groups:`** combine permissively — a user
  qualifies if they're either named in `approvers:` OR a member of any
  group in `approver_groups:`. Empty on both sides means "any authenticated
  user".
- **`required:`** is the quorum. Defaults to 1 (legacy single-approver
  behaviour). A parser-side check rejects values that exceed the combined
  size of `approvers + approver_groups` so an un-passable gate surfaces at
  apply time.
- **Reject always wins** — any single rejection from an allowed user ends
  the gate immediately, regardless of how high `required` is.
- Groups are managed at `/admin/groups` (admin-only). Rename is safe:
  gates store group **names** so a rename propagates cleanly.

### Plugin vs. script job

- **Script job** has `script:` list → tasks run inside `image`.
- **Plugin job** has `settings:` (and no `script:`) → the whole container *is*
  the task. Settings become `PLUGIN_<UPPERCASE>` env vars. Compatible with
  Woodpecker plugins.

### Needs vs. stages

- **stages** give the *big picture ordering* (humans read this).
- **needs** inside a stage let you *skip waiting* for sibling jobs (GitLab-style DAG).

### Test reports

Jobs that run tests can declare glob patterns pointing at JUnit/xUnit XML
reports the agent should parse and ship back for the Tests tab on the run
detail page:

```yaml
jobs:
  unit:
    stage: test
    image: golang:1.23
    script:
      - go install gotest.tools/gotestsum@latest
      - gotestsum --junitfile=junit.xml ./...
    test_reports:
      - junit.xml
      - "**/junit-*.xml"
```

Globs are workspace-relative. The agent parses every match after the task
loop (success or failure — a red build still writes XML for the cases that
ran) and sends one TestResultBatch per job. Per-field size caps (64 KiB)
and a whole-batch cap (4 MiB) keep a pathological test from blowing the
wire; server-side ingest re-clamps defensively.

### Templates via `extends:`

A job whose name starts with `.` is a **template** — it never runs on its own,
but other jobs in the same file can inherit from it via `extends:`:

```yaml
jobs:
  .base-go:                         # template, not materialized
    stage: test
    image: golang:1.23
    script: [make test]
    secrets: [CODECOV_TOKEN]

  unit:
    extends: .base-go               # inherits image + script + secrets

  unit-race:
    extends: .base-go
    image: golang:1.24              # child scalar wins
    script: [make test-race]        # child list replaces
```

Merge rules (GitLab-compatible):

| Field type | Rule |
|---|---|
| Scalars (`image`, `stage`, `timeout`, `retry`, `docker`, `uses`) | Child wins when set |
| Lists (`script`, `needs`, `secrets`, `tags`, `cache`, `rules`) | Child replaces parent when non-nil |
| Maps (`settings`, `with`, `variables`) | Child keys overlay parent keys |
| Structs (`artifacts`, `parallel`, `when`, `approval`) | Child wins whole |

`extends:` supports chains (`a → b → c`) with cycle detection. Cross-file
inheritance (`include:` pulling templates from another file) is not yet
supported — it's on the roadmap.

## CI variables (injected by the agent)

| Variable              | Example                          |
|-----------------------|----------------------------------|
| `CI_PIPELINE`         | `my-service`                     |
| `CI_RUN_COUNTER`      | `42`                             |
| `CI_PIPELINE_STATUS`  | `running` / `success` / `failure`|
| `CI_COMMIT_SHA`       | `abc123…`                        |
| `CI_COMMIT_BRANCH`    | `main` / `feature/xyz`           |
| `CI_PIPELINE_SOURCE`  | `webhook` / `upstream` / `manual`|
| `CI_PR_NUMBER`        | `137` (only on PR events)        |

## Rules reference

Rules are evaluated top-to-bottom; first match wins.

| Field     | Meaning                                              |
|-----------|------------------------------------------------------|
| `if`      | CEL-like expression over CI_* vars. Truthy → match.  |
| `changes` | Match if any path changed in the triggering commit.  |
| `exists`  | Match if any path exists in the workspace.           |
| `when`    | What to do when matched: `always` (default), `manual`, `never`, `on_success`, `on_failure`. |
