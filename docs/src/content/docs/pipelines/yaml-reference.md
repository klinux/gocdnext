---
title: YAML reference
description: Every key the pipeline parser accepts, what it means, and where to use it.
---

`.gocdnext/<name>.yaml` is parsed by the server's pipeline parser
with strict-unknown-fields mode (`KnownFields(true)`). This page
catalogs every accepted key with its shape, default, and a short
example. Any key not on this list is rejected at apply time — typos
surface as errors instead of being silently ignored.

## Top-level

```yaml
name: ci                    # string; defaults to filename without ext

when:                       # pipeline-level trigger gate; see "Triggers"
  event: [push, pull_request]
  branch: [main]            # singular — branch:, not branches:

stages: [lint, test, build] # ordered list; each stage waits for the
                            # previous to fully succeed

materials:                  # extra checkouts beyond the implicit one;
  - upstream:               # see "Materials" below
      pipeline: ci-server
      stage: test
      status: success

services:                   # pipeline-wide sibling service containers; all jobs
  - name: postgres          # in the run can reach them by name; see
    image: postgres:16      # "Services" below

variables:                  # env vars merged into every job
  CGO_ENABLED: "0"

concurrency: parallel       # "parallel" (default) or "serial"

notifications:              # post-run hooks; see "Notifications"
  - on: failure
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    with: { ... }

jobs:                       # map; keys are job names
  unit:
    ...
```

There is no top-level `description:` key. Surface descriptions on
the project / pipeline UI come from the project record, not the
pipeline YAML.

## Triggers (`when:`)

Controls which SCM events materialise into runs at the pipeline
level. The parser accepts:

| Key | Type | Default | Notes |
|---|---|---|---|
| `event` | `[]string` | `[push]` | `push`, `pull_request`, `tag`, `manual`, `cron`, `upstream` |
| `branch` | `[]string` | all | Singular `branch:` (the YAML key). List of branch names; substring/exact match per scheduler config. Empty = any branch. |

```yaml
when:
  event: [push, pull_request]
  branch: [main, develop]
```

`event: [manual]` makes the pipeline runnable only via *Run latest*
in the UI or `gocdnext run <pipeline>`. `event: [cron]` is set
automatically by the project's cron schedule.

`when.status:` is **rejected at parse** (it was reserved and
unenforced — declaring it gated nothing; issue #40). Tag-name regexes
aren't wired; use one pipeline per trigger shape if you need them.

### Path filtering (`when.paths`)

```yaml
when:
  event: [push, pull_request]
  paths:
    - "**/*.go"
    - "go.mod"
    - "web/**"
```

The pipeline fires only when at least one changed file of the
triggering event matches one glob (doublestar grammar — the same
`artifacts:` uses; repo-relative, `**` crosses directories). Globs
are validated at apply time. Monorepos use this to keep backend
pushes from spinning frontend pipelines and vice versa.

Where the changed-file set comes from, per event:

| Event | Source | Caveat |
|---|---|---|
| push (GitHub/GitLab) | webhook payload commit file lists | payload caps at 20 commits — bigger pushes **fail open** |
| push (Bitbucket) | — | payload has no file lists — always **fails open** |
| pull_request (GitHub) | PR files API (paginated, up to 3000 files) | needs repo credentials (PAT or GitHub App); without them, **fails open** |
| pull_request (GitLab/Bitbucket) | — | adapter not implemented yet — **fails open** |
| tag / manual / cron / upstream | — | no changed-file concept — always runs |

**Fail open** means the pipeline runs anyway: an unknown file set
must never suppress a legitimate run — extra runs are noise,
missing CI on a real change is an incident. A delivery whose
pipelines were all filtered is acknowledged with
`filtered_by_paths` in the response body and creates no run rows.

### Skipping CI from the commit message

A branch or tag push whose head-commit message contains
`[skip ci]`, `[ci skip]` or `[no ci]` (case-insensitive, anywhere
in the message) creates **no runs** — the delivery is acknowledged
with status `skipped`, visible in *Settings → Webhooks*. This is
how a job that commits back to its own repo (GitOps image-tag
bumps, changelog regeneration) avoids retriggering itself.

Two deliberate boundaries:

- **`pull_request` events never honor the markers** — otherwise any
  contributor could bypass PR validation by writing the marker into
  their own commits.
- **Annotated tags can't be skipped**: the push payload carries no
  commit message for them (same caveat GitHub Actions has).
  Lightweight tags honor the marker on the tagged commit.

Config sync still observes skipped pushes — a `[skip ci]` commit
that edits `.gocdnext/` updates the project's pipelines; it just
doesn't run them.

## Materials

Extra checkouts beyond the implicit project material. Each entry is
one of `git`, `upstream`, `cron`, or `manual`.

### `git` — additional repository

```yaml
materials:
  - git:
      url: https://github.com/org/shared-libs
      branch: main
      on: [push, tag]          # push | pull_request | tag — see "Triggers" event list
      poll_interval: 5m        # optional polling fallback
      auto_register_webhook: true
      secret_ref: SHARED_REPO_TOKEN
```

Cloned into a deterministic per-material subdirectory under
`/workspace`. The agent threads the directory into the task
container's working directory automatically — pipelines don't pick
the path.

### `upstream` — depend on another pipeline

```yaml
materials:
  - upstream:
      pipeline: ci-server
      stage: test
      status: success
```

When `ci-server.test` finishes successfully, the platform creates a
downstream run **with the same revision** in the same project. This
is gocdnext's fanout primitive — it's how monorepos chain pipelines.
The downstream's `cause` is `upstream`; the dashboard shows a
banner linking back to the trigger run.

### `cron` — scheduled trigger

```yaml
materials:
  - cron:
      expression: "0 7 * * 1-5"   # weekdays at 07:00 server-local
```

The cron expression is parsed by the same library used for
project-level crons. The run's `cause` is `cron`.

### `manual` — only via UI/CLI

```yaml
materials:
  - manual: true
```

Equivalent to `when.event: [manual]` at pipeline level. Runs are
created only via *Run latest* or `gocdnext run`.

## Stages

```yaml
stages: [lint, test, build, deploy]
```

Ordered list. Stage *N+1* dispatches when every job in stage *N*
hits a terminal status:

- All `success` → next stage runs.
- Any `failed` → run is marked failed; stages past N are skipped.
- Any `awaiting_approval` → run holds at the boundary until
  approved (see [Approval gates](#approval-gates)).

## Jobs

```yaml
jobs:
  <name>:
    stage: <stage-name>        # required, must be in `stages:`
    image: alpine:3.20         # OR `uses:` (mutually exclusive)
    script:                    # shell lines; requires `image:`
      - go test ./...
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1       # plugin reference; mutually exclusive
    with:                      # inputs passed as PLUGIN_* env to the image
      command: test ./...
    needs: [other-job]         # ordering inside the same stage
    needs_artifacts:
      - from_job: deps
        paths: [node_modules]
        dest: ./
    docker: true               # mounts docker socket / DinD sidecar
    cluster: prod-gke          # registered deploy-target cluster; injects
                               # its kubeconfig as PLUGIN_KUBECONFIG
    variables:
      MY_VAR: hello
    secrets: [SLACK_TOKEN]
    cache:
      - key: ...
        paths: [...]
    artifacts:
      paths: [...]
      optional: [...]
      expire_in: 30d
      when: on_success
    test_reports: ["**/junit.xml"]
    parallel:
      matrix:
        - GO_VERSION: ["1.24", "1.25"]
          OS: [ubuntu, alpine]
    timeout: 30m
    retry: 2
    tags: [linux, x86_64]
    agent:
      profile: gpu
      tags: [linux, x86_64]
    resources:
      requests: { cpu: "100m", memory: 256Mi }
      limits:   { cpu: "2",    memory: 4Gi }
    approval:
      description: "Promote to prod"
      approver_groups: [release-approvers]
      required: 2
    outputs:                           # values downstream jobs reference via
      next: NEXT                       # ${{ needs.<this-job>.outputs.<alias> }}
      kind: KIND                       # plugin/script writes KEY=value to $GOCDNEXT_OUTPUT_FILE
```

Per-key:

| Key | Type | Notes |
|---|---|---|
| `stage` | string | required |
| `image` | string | container image; mutually exclusive with `uses:` |
| `script` | `[]string` | shell lines run inside `image:` |
| `uses` | string | plugin reference: `gocdnext/<name>@v1` or `ghcr.io/...@v2` |
| `with` | map | inputs passed as `PLUGIN_*` env to the plugin image |
| `needs` | `[]string` | other jobs in the same stage that must finish first |
| `needs_artifacts` | list | tar of upstream job's artefacts restored before run |
| `docker` | bool | mount docker socket / DinD — for testcontainers, buildx, etc. |
| `cluster` | string | names a [registered deploy-target cluster](#cluster-target-cluster); injects its kubeconfig as `PLUGIN_KUBECONFIG` (masked). Mutually exclusive with `with.kubeconfig`. |
| `variables` | map | env vars merged into the job's environment |
| `secrets` | `[]string` | project/global secrets injected as env, masked in logs |
| `cache` | list | tar paths between runs, keyed by template string |
| `artifacts` | block | files to ship to the artefact backend (see [Artifacts](#artifacts)) |
| `test_reports` | `[]string` | JUnit XML globs surfaced in the Tests tab |
| `parallel.matrix` | list | expand the job into one cell per cartesian product |
| `parallel.count` | int | run N identical copies (no matrix) |
| `rules` | — | **rejected at parse** — was accepted-but-unenforced; use `when.paths` / `approval:` (issue #40) |
| `timeout` | duration | hard kill after — `30m`, `2h` |
| `retry` | int | retry count on `failed` |
| `tags` | `[]string` | extra constraints unioned with the agent profile |
| `agent` | block | runner profile + extra tags |
| `resources` | block | requests + limits, validated against the profile's max |
| `approval` | block | manual gate (see [Approval gates](#approval-gates)) |

`image` + `uses` are mutually exclusive — the parser rejects both
on the same job. `approval:` is exclusive with `image`/`uses`/
`script`/`artifacts` — an approval job parks the run, it doesn't
execute anything.

## Cache

```yaml
cache:
  - key: pnpm-store-${CI_COMMIT_BRANCH}
    paths: [.pnpm-store, node_modules]
```

`key:` is a template — variables expanded:

| Variable | Expands to |
|---|---|
| `${CI_COMMIT_BRANCH}` | branch name (sanitised) |
| `${CI_PIPELINE_NAME}` | pipeline name |
| `${CI_PROJECT_SLUG}` | project slug |
| `{{ hash "<glob>" }}` | hex digest of the sorted files matching the glob (workspace-relative) |

`{{ hash "<glob>" }}` is the closed grammar for content-keyed
caches. Single-pass evaluation — the result is not re-expanded —
and the glob is workspace-relative with no `..` traversal. Useful
for lockfile-keyed caches:

**Cache `paths:` are relative to the job's working directory** —
which is the material's checkout subdirectory
(`/workspace/src/<hash>`), NOT the workspace mount root. Tools
whose cache env var demands an absolute path (Go's `GOCACHE` /
`GOMODCACHE`) must derive it from the working dir at runtime:

```yaml
script:
  - export GOMODCACHE="$PWD/.go-cache/mod" GOCACHE="$PWD/.go-cache/build"
  - go test ./...
cache:
  - key: go-ci
    paths: [.go-cache]
```

Pointing such variables at the mount root (`/workspace/.go-cache`)
writes OUTSIDE what the tar captures — the bucket uploads empty
(a few bytes) and every run re-downloads from scratch.

```yaml
cache:
  - key: pnpm-store-{{ hash "pnpm-lock.yaml" }}
    paths: [.pnpm-store]
```

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
  optional:
    - "**/coverage.xml"          # publish if present, no-op if missing
  expire_in: 30d                 # optional; global default otherwise
  when: on_success               # on_success | on_failure | always
```

`paths:` is required to exist — the job fails if a listed path is
missing. `optional:` (a bare list of paths) is "publish if present,
no-op if missing" — useful for coverage reports that conditional
jobs may or may not generate. A path that appears in both `paths:`
and `optional:` is treated as required (required wins).

`expire_in:` is a duration (`24h`, `30d`); empty falls back to the
global retention default set by the operator. `when:` decides
whether the upload runs at all: `on_success` (default), `on_failure`
(only when the job failed — useful for crash dumps), or `always`.

## Test reports

```yaml
test_reports:
  - "**/junit.xml"
  - "build/test-results/**/*.xml"
```

`test_reports:` is a **bare list** of globs (not a `{paths: [...]}`
block). The matched files are parsed as JUnit/xUnit XML at job
completion and populate the run's *Tests* tab — per-case status,
duration, failure message + stack trace.

## Coverage reports

```yaml
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["go test -coverprofile=coverage.out ./..."]
    coverage_report:
      path: coverage.out
      format: go-cover          # go-cover | lcov | cobertura | jacoco
      fail_under: 70            # optional gate — see below
    artifacts:
      optional: [coverage.out]  # keep the raw file too, if you want it
```

After tasks complete (success OR failure — a red test run still
produced a valid profile), the agent parses the declared file and
ships ONLY the summary: total lines covered/total plus a per-package
breakdown capped at 200 entries (worst coverage first; truncation is
announced in the job log, totals stay exact). The run page gains a
**Coverage** tab — per-job percentage, package breakdown, and a
trend sparkline per job across the pipeline's recent runs.

Formats: `go-cover` (`go test -coverprofile`, counted in statements
— the same unit `go tool cover -func` reports), `lcov`
(vitest/jest/nyc), `cobertura` XML (coverage.py, .NET coverlet),
`jacoco` XML (JVM — Gradle/Maven Java/Kotlin; counted in lines, the
JaCoCo "Lines" metric).

**Delta vs main**: every coverage card (and the GitHub check-run
summary, when the Checks integration is on) shows the movement
against the latest MAINLINE measurement of the same job series —
mainline meaning branch-push (`webhook`) and poll-discovered runs;
tag/PR/manual/cron runs never become baselines. `−1.2pp vs main`
is the number a PR reviewer actually wants. The first run of a
series has no baseline and shows none. Pipelines registering
multiple push branches mix them in the baseline — single-branch
mainlines (the common shape) are exact.

**`fail_under` (optional gate, default off)**: when set (0–100],
the JOB FAILS if total coverage lands below the threshold —
at-threshold passes. Gating is bypass-proof: with `fail_under`
set, a missing, oversized, or unparsable coverage file also fails
the job (a gate that passes when its evidence is deleted is not a
gate). Without `fail_under`, those same conditions log an error
and report nothing — reporting never gates by accident.

## Parallel / matrix

```yaml
jobs:
  build:
    parallel:
      matrix:
        - OS: [linux, darwin]
          ARCH: [amd64, arm64]
    image: golang:1.23
    script:
      - GOOS=$OS GOARCH=$ARCH go build ./...
```

`parallel.matrix:` is a **list of objects**, each object mapping
dimension names to value lists. The cartesian product across all keys
in all entries is expanded into one job per cell. Above: 2 × 2 = 4
jobs.

Each dimension is injected into the cell's environment as its own
variable — `OS: [linux]` exposes `$OS=linux` — plus a combined
`GOCDNEXT_MATRIX="ARCH=amd64,OS=linux"` (dimensions sorted,
`,`-joined). Where you can read them:

- **`script:`** — directly at runtime: `GOOS=$OS GOARCH=$ARCH go build`.
- **plugin `with:`** — via `${{ OS }}`; plugin settings resolve
  variable refs, and a dimension is a variable.
- **NOT another `variables:` entry** — one variable can't reference
  another (matrix or not, by design); read the dimension as `$OS` at
  runtime instead.
- **NOT `image:`** — the image string is sent verbatim, no ref of any
  kind is substituted there. To vary behaviour per cell, branch in
  `script:` via `$OS` rather than templating the `image:` field.

Constraints (rejected at apply): a dimension name must be a valid env
identifier, must not use the reserved `CI_` / `GOCDNEXT_` prefix, and
must not collide with a `variables:`, `secrets:`, or `id_tokens:` name
(that would make `$NAME` ambiguous); values can't contain `,` or `=`
(the matrix-key separators).

Use `parallel.count: N` instead of `matrix:` to run N identical
copies without per-cell substitution.

Failure of one cell does NOT stop sibling cells; the run aggregates
`success` only when every cell succeeds.

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
    script:
      - psql -h postgres -U postgres -c 'select 1'
```

`services:` is pipeline-level. Every declared service spins up
alongside the run and is reachable from every job by its `name:`
as a DNS alias. There is no per-job opt-in — declare a service only
if you want it available run-wide.

`name:` defaults to the image's short name when omitted (`image:
postgres:16` ⇒ `postgres`). `image:` is required.

### Lifecycle

The control plane tracks one `service_run` row per service per run
with these states:

| Status | Meaning |
|---|---|
| `starting` | Container/Pod created; waiting for the ready signal |
| `ready` | Docker: container running. K8s: Pod `phase=Running`. |
| `failed` | Crashed before ready — the run is marked failed. |
| `stopped` | Cleanly torn down at run end (happy-path terminal). |

See [Services lifecycle](/gocdnext/docs/concepts/services/) for the
concept-level walkthrough.

## Secrets

```yaml
jobs:
  deploy:
    secrets: [SSH_DEPLOY_KEY, SSH_KNOWN_HOSTS]
    uses: ghcr.io/klinux/gocdnext-plugin-ssh@v1
    with:
      key: ${{ SSH_DEPLOY_KEY }}
      known_hosts: ${{ SSH_KNOWN_HOSTS }}
```

Secrets are AES-256-GCM-encrypted at rest. Listed names are
resolved at dispatch time from the project's secret store
(*Project → Secrets*), injected as env vars, and masked in
streamed log lines. Global secrets fall through when a project
secret with the same name doesn't exist.

### Substitution grammar

The reference grammar inside `with:`, `variables:`, and other
template fields is intentionally tight:

- `${{ NAME }}` — hard reference. Resolved at dispatch against
  secrets first, then `variables`. **Identifier only** — dotted
  forms (`${{ secrets.X }}`, `${{ matrix.GO_VERSION.0 }}`),
  function calls, and operators are rejected with "unsupported
  reference expression". Unresolved references fail the dispatch
  with the reference **name** (never the value of something else),
  so secret values can't leak via error message.
- `${VAR}` — soft, shell-style. Passed through to the container
  and expanded by the shell at runtime. Use this for env vars the
  agent or runtime injects (`${CI_COMMIT_BRANCH}`, etc.).

Substitution is single-pass: the result of one reference is never
re-expanded, so a chain like `${{ A }}` → `${{ B }}` does not
recurse.

### CI built-ins

The agent injects these into every job's environment:

| Name | Value |
|---|---|
| `CI` | `true` |
| `GOCDNEXT` | `true` |
| `CI_BRANCH` | branch the run is on |
| `CI_COMMIT_BRANCH` | alias for `CI_BRANCH` |
| `CI_COMMIT_SHA` | full revision SHA |
| `CI_COMMIT_SHORT_SHA` | 8-char prefix of the SHA |
| `CI_RUN_COUNTER` | monotonically-increasing per-pipeline run number |
| `CI_RUN_ID` | UUID of the run |
| `CI_PIPELINE_ID` | UUID of the pipeline |
| `CI_PIPELINE_NAME` | pipeline name |
| `CI_PROJECT_ID` | UUID of the project |
| `CI_PROJECT_SLUG` | project slug |
| `CI_JOB_NAME` | job name |
| `CI_CAUSE` | trigger that created the run — `webhook`, `pull_request`, `tag`, `manual`, `upstream`, `schedule`, `poll` |
| `GOCDNEXT_MATRIX` | matrix jobs only — the cell as `K=V,K=V` (dimensions lex-sorted). Each dimension is **also** injected as its own var, e.g. `$OS`, `$ARCH`. See [Parallel / matrix](#parallel--matrix). |

### Pull-request runs

When `CI_CAUSE == "pull_request"`, the following are also injected
from the webhook payload (server-side, no operator configuration):

| Name | Value |
|---|---|
| `CI_PULL_REQUEST_KEY` | PR number (e.g. `1234`) |
| `CI_PULL_REQUEST_BRANCH` | head ref (e.g. `feature/foo`) |
| `CI_PULL_REQUEST_BASE` | base ref (e.g. `main`) |
| `CI_PULL_REQUEST_TITLE` | PR title |
| `CI_PULL_REQUEST_AUTHOR` | PR author handle / email |
| `CI_PULL_REQUEST_URL` | full URL of the PR |

Missing fields stay UNSET (rather than empty strings) — so a PR
with no title leaves `${CI_PULL_REQUEST_TITLE}` literal at
substitution time. Non-PR runs (push, manual, upstream, schedule,
poll) skip all `CI_PULL_REQUEST_*` vars silently.

### Tag-push runs

When `CI_CAUSE == "tag"`, the following are injected from the
webhook payload:

| Name | Value |
|---|---|
| `CI_TAG_NAME` | tag name (e.g. `v1.2.3`) — always present on tag runs |
| `CI_TAG_MESSAGE` | head commit message of the tagged commit — lightweight tags only; annotated tags omit |
| `CI_TAG_AUTHOR` | head commit author — lightweight tags only |

`CI_BRANCH` carries the tag name on tag runs (the agent does a
detached-HEAD checkout at the tag). `CI_COMMIT_SHA` carries the
git SHA the tag points at — use it for materials/manifests that
need to pin to a specific commit. Note that this is the **git
ref target SHA** (40-hex SHA-1), NOT an OCI image digest — if you
need to anchor a cosign signature to a specific image manifest,
either let cosign resolve the tag at sign time (it anchors to the
manifest digest internally — see the
[trunk-based release recipe](/gocdnext/docs/concepts/trunk-based-release/))
or have the build job emit the buildx-produced digest as a job
output and pass it into the sign step.

Annotated-tag webhooks lack a head_commit so `CI_TAG_MESSAGE` +
`CI_TAG_AUTHOR` are omitted — the substitution layer keeps those
literal.

### Upstream runs

When `CI_CAUSE == "upstream"` (a run created by an `upstream`
material's fanout — see [Materials](#materials)), the **triggering**
pipeline's identity is injected:

| Name | Value |
|---|---|
| `CI_UPSTREAM_PIPELINE` | name of the pipeline that triggered this run |
| `CI_UPSTREAM_RUN_COUNTER` | the upstream run's `CI_RUN_COUNTER` |
| `CI_UPSTREAM_STAGE` | the upstream stage whose success triggered the fanout |

The common use is rebuilding the **exact** version/tag the upstream
produced. Run counters are per-pipeline (the downstream's own
`CI_RUN_COUNTER` differs from the upstream's), so a deploy that wants
the image the build job tagged uses the upstream's counter:

```yaml
# build pipeline — tags the image
tags: 1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}

# deploy pipeline (upstream of build) — references the SAME tag
image: registry/app:1.${CI_UPSTREAM_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}
```

`CI_COMMIT_SHORT_SHA` already matches across the two (fanout runs the
downstream on the same revision). Non-upstream runs skip all
`CI_UPSTREAM_*` vars silently.

To wire a pipeline to tag pushes, declare it in `when.event`:

```yaml
name: release
when:
  event: [tag]
```

Tag-listening pipelines match the repo by URL only (branch is
irrelevant — a tag points at a SHA that may not be on any branch).
Pipelines that don't opt into `tag` are silently skipped on a
tag push.

These are also available as `${{ NAME }}` references and
`${NAME}` shell-style env vars (the latter expanded by the shell
inside the container).

## OIDC id_tokens (`id_tokens:`)

Per-job OIDC JWTs for keyless cloud auth — exchange them for GCP /
AWS / Azure / Vault credentials via workload identity federation
instead of storing service account keys in `secrets:`.

```yaml
jobs:
  deploy:
    stage: ship
    image: google/cloud-sdk:slim
    id_tokens:
      GCP_ID_TOKEN:
        aud: https://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ci/providers/gocdnext
      VAULT_JWT:
        aud: [https://vault.example.com, https://vault-dr.example.com]
    script:
      - ./deploy.sh   # $GCP_ID_TOKEN and $VAULT_JWT hold signed JWTs
```

- Map key = env var name (POSIX charset; `CI_`/`GOCDNEXT_`
  prefixes reserved; collisions with pipeline- or job-level
  `variables:` and the job's `secrets:` rejected at apply).
- Not allowed on approval gates (they never dispatch a
  container, so the token would never be minted).
- `aud` is **required** — scalar or list, must match the cloud
  trust config's expected audience exactly.
- Token values are auto-added to the job's log masks.
- Requires the server to have `publicBase` + `secretKey`
  configured; otherwise the job fails loud at dispatch.
- Pull-request runs get a deliberately ref-less `sub`
  (`project:X:pipeline:Y:pull_request`) so branch-pinned cloud
  policies exclude PRs by construction.

Claims, `sub` grammar, per-provider trust snippets, TTL and key
rotation live in [OIDC id_tokens](/concepts/id-tokens/).

## Approval gates

```yaml
jobs:
  promote-prod:
    stage: deploy
    approval:
      description: "Promote build to production"
      approver_groups: [release-approvers]
      required: 2
```

| Key | Type | Notes |
|---|---|---|
| `description` | string | shown in the approval modal |
| `approvers` | `[]string` | explicit allow-list; each entry is matched against the deciding user's display **name** OR **email**; empty = "any authenticated user" |
| `approver_groups` | `[]string` | gate on group membership (matched by user id — robust to name/email changes); union with `approvers` |
| `required` | int | quorum (default `1`) — distinct allowed approvers needed before the gate passes |

Approval jobs park at `awaiting_approval` until the quorum is met.
A reject from any allowed user fails the gate immediately. The
parser rejects mixing `approval:` with `image:`, `uses:`, `script:`,
or `artifacts:` — an approval job is a gate, not an executor.

See [Approval gates](/gocdnext/docs/concepts/approvals/) for the
deeper walk-through.

## Deployments (`deploy:`)

Mark an executable job as a deployment to a named environment. The
job still runs your real deploy `script:` / `uses:` — `deploy:` is a
**tracking marker**, not an executor. When the job succeeds, gocdnext
records that this run shipped `version` to `environment` and surfaces
it in the Environments tab (current version, history, one-click
rollback).

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
| `environment` | string (required) | Target environment. Lazy-created on first deploy — no pre-registration. |
| `version` | string (optional) | Version recorded as deployed. Refs allowed (`${{ needs.X.outputs.Y }}`, `${{ CI_* }}`, `${CI_*}`), resolved against CI vars **only, never secrets**. Omitted → defaults to `CI_COMMIT_SHORT_SHA`. A reference that can't resolve fails the job terminally at dispatch. |

`deploy:` is rejected on an `approval:` job — a gate doesn't deploy.
Gate a deploy with an approval on a *separate* upstream job. A deploy
job's success IS the deployment's success.

See [Deployments & rollback](/gocdnext/docs/concepts/deployments/) for
the tracking model, environments, and rollback semantics.

## Cluster target (`cluster:`)

Name a registered Kubernetes deploy-target cluster. At dispatch the
scheduler resolves the name to its stored kubeconfig and injects it
as `PLUGIN_KUBECONFIG` (masked in logs), so the kubectl / helm /
kustomize plugins authenticate without a pasted kubeconfig.

```yaml
jobs:
  deploy-prod:
    stage: ship
    uses: ghcr.io/klinux/gocdnext-plugin-kubectl@v1
    with:
      command: "apply -k k8s/"
    cluster: prod-gke
```

| Key | Type | Notes |
|---|---|---|
| `cluster` | string | name of a cluster registered by an admin (*Settings → Clusters*). Injects that cluster's kubeconfig as `PLUGIN_KUBECONFIG`, masked. The **single source** of `PLUGIN_KUBECONFIG` — the parser rejects a job that also sets `with.kubeconfig` or otherwise defines `PLUGIN_KUBECONFIG` (via `variables`, `secrets`, `id_tokens`, or a `parallel.matrix` dimension). Not allowed on an `approval` gate (a gate dispatches nothing). |

The cluster must exist at **apply** time — `cluster: prod-gke`
naming an unregistered cluster fails the apply with a message citing
`prod-gke`. Whether *this project* may use the cluster (the
per-cluster `allowed_projects` allow-list) is enforced at
**dispatch**. Both errors name the cluster, never its credential.

See [Cluster registry](/gocdnext/docs/concepts/clusters/) for the
three auth types (`kubeconfig` / `token` / `in_cluster`), governance,
and the in-cluster ServiceAccount setup.

## Job outputs (`outputs:`)

A job declares structured values it promises to produce; downstream
jobs reference them via `${{ needs.<job>.outputs.<alias> }}`
resolved at dispatch — no `needs_artifacts:` + `source` plumbing
required.

```yaml
jobs:
  bump:
    stage: tag
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      next: NEXT       # alias: env-var name written by the plugin
      kind: KIND       # to $GOCDNEXT_OUTPUT_FILE

      # Object form — opt-in log masking (issue #22, v0.15.3+).
      # The resolved value gets added to the downstream job's
      # LogMasks even when it's under the 8-char heuristic
      # threshold. See "Outputs are NOT a secret channel" below.
      release-token:
        env: RELEASE_TOKEN
        masked: true

  publish:
    stage: deploy
    needs: [bump]
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    with:
      image: ghcr.io/org/app
      tags: ${{ needs.bump.outputs.next }}
```

### How values get there

The agent injects `$GOCDNEXT_OUTPUT_FILE` into the job's env —
a private path the runner picks, never operator-controlled. The
plugin (or a `script:` step) writes `KEY=value` lines to that
file. At job end the agent parses the file, filters to the keys
declared in `outputs:`, rekeys to the YAML aliases, and ships
them in `JobResult`. Storage is a JSONB column on `job_runs`
written in the same transaction as the success flip — so the
scheduler's `${{ needs.X.outputs.Y }}` lookup at dispatch always
sees a consistent snapshot.

### Validation

- Aliases (LHS of the map): `[a-z][a-zA-Z0-9_-]*` — lowercase-
  leading per gocdnext idiom. Case-sensitive end-to-end:
  `${{ needs.X.outputs.Next }}` does not resolve `outputs.next`.
- Env names (RHS): POSIX env-var shape `[A-Za-z_][A-Za-z0-9_]*` —
  what the plugin writes to the output file.
- Cap: 64 entries per job (declaration); 64 KB total payload
  (sum of key + value bytes). Both enforced agent-side AND
  server-side.
- Outputs are part of the build CONTRACT — if declared, the
  plugin MUST write each one. Missing keys fail the job loud
  with a message that cites the alias and the env name expected.
  Pipelines like `gocdnext/semver-bump` and `gocdnext/image-copy`
  write a superset, so declaring a subset is fine; extras are
  silently dropped.

### Outputs are NOT a secret channel

Use `secrets:` for credentials. Outputs are designed for non-
sensitive small values (versions, digests, deploy URLs) and the
substituted value WILL appear in logs of any downstream step that
prints the env var or argv that uses it.

Defence in depth, in two layers:

1. **Heuristic auto-mask (scheduler).** The scheduler adds every
   resolved output value of length ≥ 8 to the downstream job's
   LogMasks list automatically. That's a safety net for
   "operator forgot the value was a token" — not the recommended
   path. Short values (< 8 chars) skip the heuristic to avoid
   false-positive substring replacements across unrelated log lines.

2. **Opt-in mask (operator, issue #22).** The object form
   `alias: {env: NAME, masked: true}` flags an output as
   sensitive, bypassing the 8-char scheduler heuristic — the
   resolved value lands in LogMasks regardless. This is the
   right escape hatch for a 4-7 char token that the heuristic
   would skip.

   ```yaml
   outputs:
     release-token:
       env: RELEASE_TOKEN
       masked: true
   ```

   Schema is strict on the object form: a typo like `mask:`
   (missing `e`) or `env_var:` fails parse with an "unknown key"
   error. Accepted keys are `env` and `masked`.

   **4-char floor still applies.** The agent's log replacer
   skips masks shorter than 4 characters so common short tokens
   ("go", "v1") aren't globally rewritten. There is no log
   redaction at all for values under 4 chars — `secrets:` hits
   the same runner floor when echoed, so the recommendation for
   a short-and-sensitive value is to NOT treat it as a build
   output at all: take it from `secrets:` directly (it stays out
   of the outputs persistence + downstream substitution surface)
   and avoid echoing it in any step that prints env or argv.
   Masking applies to agent log streams only — the persisted
   output value is propagated verbatim to downstream
   `${{ needs.X.outputs.Y }}` substitutions.

### Substitution scope

`${{ needs.X.outputs.Y }}` substitution happens in `env:` /
`variables:` / plugin `with:`. It does NOT run on raw `script:`
lines — shell-side `${VAR}` references stay verbatim so the
inner shell can resolve them at runtime. The pattern when a
script needs an output value: land it via `variables:` and
reference as `$NAME` inside the script. Example:

```yaml
create-tag:
  needs: [bump]
  image: alpine:3.20
  variables:
    NEXT: ${{ needs.bump.outputs.next }}   # resolved at dispatch
  script:
    - git tag -a "$NEXT" -m "Release"     # shell expansion at runtime
```

### Matrix selector (`${{ needs.X.matrix[KEY].outputs.Y }}`)

Issue #21. When the upstream job declares a `strategy.matrix`,
it expands into one job_run per combination. Bare
`${{ needs.X.outputs.Y }}` against such an upstream errors loud —
the scheduler can't pick "the right one". The downstream picks
explicitly via the matrix selector:

```yaml
jobs:
  bump:
    strategy:
      matrix:
        shard: [apac, emea, us]
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      next: NEXT

  publish-apac:
    needs: [bump]
    image: alpine:3.20
    variables:
      # 1-dim shortcut — `shard` is the only dimension
      TAG: ${{ needs.bump.matrix[apac].outputs.next }}
    script:
      - git tag -a "$TAG" -m "Release APAC"
```

Three selector forms accepted:

| Form | When to use |
|---|---|
| `matrix[VALUE]` | 1-dim shortcut. Only valid when the upstream's `strategy.matrix` has exactly one dimension; expanded to `dim=VALUE` at resolution time. |
| `matrix[K=V]` | Explicit 1-dim. Stable shape if you might add a second dimension later. |
| `matrix[K1=V1,K2=V2]` | Multi-dim. Order doesn't matter — the resolver lex-sorts both your selector and the stored row key before comparing, so `matrix[arch=amd64,os=linux]` and `matrix[os=linux,arch=amd64]` resolve to the same row. |

Errors at dispatch (loud, the downstream job is failed before
the agent ever sees the assignment):

- 1-dim shortcut against a multi-dim upstream → "use the explicit form matrix[k=v,...]"
- Unknown dimension name in selector → cites the declared dimensions
- Selector value doesn't match any row (matrix `exclude:` removed it, or typo) → cites the available canonical keys
- Selector against a NON-matrix upstream → "drop the matrix[...] selector and use the bare form"

Out of scope (separate issues if demand surfaces):

- Aggregation: `${{ needs.X.outputs.next[*] }}` (a list across all matrix rows).
- Reduce expressions: piping through `| join(",")` or similar.

### Kubernetes isolated mode parity (v0.12+)

Both `agent.workspace.accessMode` values (`ReadWriteMany` shared
mode and `ReadWriteOnce` isolated mode) support `outputs:`
identically since v0.12.0. The isolated path reads
`$GOCDNEXT_OUTPUT_FILE` via housekeeper exec on the ephemeral
pod; semantics, caps, and validation are unchanged. No workaround
needed.

### Compat

Plugins like `gocdnext/semver-bump` and `gocdnext/image-copy`
emit BOTH the new `$GOCDNEXT_OUTPUT_FILE` AND the legacy
workspace file (`.gocdnext/semver.env`, `.gocdnext/image-copy.env`)
in parallel. Pipelines on older gocdnext agents (pre-v0.11) keep
working via `needs_artifacts:` + `source`. New pipelines should
prefer the native syntax.

## Notifications

```yaml
notifications:
  - on: failure
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#ci-alerts"
    secrets: [SLACK_WEBHOOK]
  - on: success
    uses: ghcr.io/klinux/gocdnext-plugin-discord@v1
    with: { ... }
```

`on:` accepts `failure`, `success`, `always`, `canceled` (single
'l' — the parser canonical form). The notification dispatches as a
synthetic job after the main run terminates; its log appears in
the run-detail page like any other job.

## Agent selection

```yaml
jobs:
  build:
    stage: build
    tags: [linux]               # job-level extra constraint
    agent:
      profile: gpu-pool
      tags: [linux, x86_64]
```

`tags:` (either job-level or under `agent:`) filter which agents
can claim the job. `agent.profile:` references a runner profile
(set in *Settings → Runner profiles*) — admins define what infra
each profile means (k8s nodeSelector, resources, image overrides);
pipelines pick by name. Profile tags + job tags + agent.tags are
unioned at apply time.

## Resources

```yaml
jobs:
  big-test:
    stage: test
    resources:
      requests: { cpu: "500m",  memory: 1Gi }
      limits:   { cpu: "2",     memory: 4Gi }
```

Mirrors `corev1.ResourceRequirements`. Empty fields fall back to
the resolved profile's defaults; non-empty fields are validated
against the profile's `max_cpu` / `max_mem` at apply time.

## Timeout

```yaml
jobs:
  long-thing:
    timeout: 2h
```

Hard kill if the job hasn't reached terminal status within the
window. Default is no timeout. Use this to protect against wedged
tests or infinite loops.
