---
title: Materials
description: How a run gets its source — the implicit project material plus the explicit git, upstream, cron, and manual entries.
---

A **material** is what creates a run. Every pipeline has at least
one (the implicit project material — the repo the project was
applied with). You can add more, and you can change which event
fires the trigger. Four kinds:

| Kind | Trigger | Use case |
|---|---|---|
| `git` (implicit) | Webhook on the project's main repo | The 80% case — push triggers run |
| `git` (explicit) | Webhook OR poll on a sibling repo | Need a second checkout |
| `upstream` | Another pipeline's stage hits success | Fanout across pipelines |
| `cron` | Schedule from project settings | Nightly builds |
| `manual` | Operator clicks "Run latest" or `gocdnext run` | Promotions, hotfixes, one-offs |

## Implicit project material

When you `gocdnext apply` a project against a git repo, the
platform records that repo as the project's `scm_source`. Every
pipeline in the project gets an implicit `git` material pointing
at it — no YAML needed. Webhooks on the SCM source create runs;
the run's working tree is the repo at the pushed SHA.

You can override the implicit material's branch/path-filter via
the pipeline's `when:`:

```yaml
when:
  branches: [main]              # only main triggers
  paths: ["server/**"]          # only when server/ changed
```

## Explicit git material

When a pipeline needs a SECOND repo cloned alongside (shared
configs, vendored modules in a separate repo, …):

```yaml
materials:
  - git:
      url: https://github.com/myorg/shared-libs
      branch: main
      path: vendor/shared-libs
      poll_interval: 5m         # optional
```

The shared-libs repo gets cloned into `vendor/shared-libs/` of
the run's workspace. Webhooks on the shared-libs repo also
trigger this pipeline (assuming the SCM source is registered
on gocdnext) — useful when the lib is the actual change driver.

`poll_interval:` is the polling fallback for SCM sources where
webhook delivery is unreliable (corporate firewalls, self-hosted
behind VPN). Format is Go duration (`5m`, `1h30m`). Empty =
webhook-only.

## Upstream material

This is gocdnext's fanout primitive — the GoCD-shaped piece. A
pipeline declares it depends on another pipeline's stage finishing
successfully:

```yaml title=".gocdnext/deploy.yaml"
name: deploy
materials:
  - upstream:
      pipeline: ci-server
      stage: test
      status: success
stages: [deploy]
jobs:
  ship:
    stage: deploy
    image: alpine
    script: ["echo deploying"]
```

When `ci-server.test` finishes successfully, gocdnext **automatically**
creates a `deploy` run with the **same revision**. Same SHA. Same
materials snapshot. The run's `cause` is `upstream`; the dashboard
shows a banner linking back to the trigger run.

This is what makes monorepo fanout safe — the deploy is always
running against the exact code the test passed on, not the latest
HEAD which might've moved.

### Multiple upstreams

A pipeline can have multiple upstream materials. The run fires
when ANY of them succeeds (OR semantics):

```yaml
materials:
  - upstream:
      pipeline: ci-server
      stage: test
      status: success
  - upstream:
      pipeline: ci-agent
      stage: test
      status: success
```

For AND semantics (wait for BOTH), you can't express it in YAML
directly — instead, have a sentinel pipeline that succeeds only
when both have, and depend on the sentinel.

### Why not just `needs:`?

`needs:` orders jobs WITHIN a single run. Upstream materials chain
ACROSS runs in different pipelines. Different scopes; both exist
because both problems are real.

## Cron material

Cron schedules are declared in the project settings UI (*Settings
→ Crons*), not in YAML. The cron entry points at a specific
pipeline + provides a cron expression + active flag.

```
Pipeline: nightly
Cron:     0 2 * * *        # 02:00 every day
Branch:   main
Active:   yes
```

When the cron fires, a run is created with `cause: cron`. The
pipeline's YAML doesn't need to reference cron at all — but you
might want to gate jobs to only run for cron causes:

```yaml
jobs:
  full-regression:
    when:
      event: [cron]
    image: ...
    script: [...]
```

`event: [cron]` makes the job fire only on cron-caused runs,
skipped on push.

## Manual material

For pipelines that should ONLY fire from the *Run latest* button
or `gocdnext run`:

```yaml
when:
  event: [manual]
```

Useful for production deploys, hotfixes, one-shot ops. The run
is auditable (who clicked, when) and pairs naturally with
[approval gates](/gocdnext/docs/concepts/approvals/) when the
operation needs a second pair of eyes.

## Revisions snapshot

Every run stores a `revisions` JSONB at create time — a snapshot
of every material's `(repo, branch, sha)` at the moment the run
was triggered. This is what lets fanout always run against the
exact same code the upstream tested.

Reading the snapshot at job runtime (so a build script can stamp
the binary version, etc.):

```yaml
jobs:
  build:
    image: alpine
    script:
      - echo "Building from $CI_COMMIT_SHORT_SHA on $CI_COMMIT_BRANCH"
      - echo "Upstream materials:"
      - env | grep ^CI_MATERIAL_
```

`CI_MATERIAL_<name>_REVISION` and `CI_MATERIAL_<name>_BRANCH`
are exposed for every material in the snapshot.

## Common pitfalls

- **Race between webhook + poll**: if a `git` material has both
  webhook AND polling configured, the same push can land twice.
  The platform dedupes by `(material_id, sha)` so the second one
  no-ops, but watch the logs the first time you set this up.
- **Upstream against a not-yet-applied pipeline**: if `deploy`
  declares an upstream on `ci-server` but `ci-server` doesn't
  exist yet, apply fails with a clear error. Apply order:
  upstream pipelines first.
- **Same SHA, different events**: a `push` on main fires the
  pipeline; a `tag` on the same commit fires it again. Different
  cause, different run. Use `when.event:` to gate which causes
  run which jobs.
- **Branch deletion**: when a branch is deleted, an upstream
  material referencing it stops firing. Always include
  `branches: [main]` or similar on production-relevant pipelines.
