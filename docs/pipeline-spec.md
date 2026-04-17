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
      secret_ref: github-app-org # k8s secret or secret-manager ref
  - upstream:
      pipeline: build-core
      stage: test
      status: success             # default "success"
  - cron:
      expression: "0 */2 * * *"
  - manual: true
```

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

### Plugin vs. script job

- **Script job** has `script:` list → tasks run inside `image`.
- **Plugin job** has `settings:` (and no `script:`) → the whole container *is*
  the task. Settings become `PLUGIN_<UPPERCASE>` env vars. Compatible with
  Woodpecker plugins.

### Needs vs. stages

- **stages** give the *big picture ordering* (humans read this).
- **needs** inside a stage let you *skip waiting* for sibling jobs (GitLab-style DAG).

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
