---
title: Cache strategies
description: "How the cache: block tar's directories between runs, key templating, what to cache for each toolchain."
---

Caches are the difference between a 30-second warm build and a
5-minute cold one. gocdnext's `cache:` block tars listed paths at
job end, stores them in the cache backend (the artefact store),
and restores them at the start of the next job that asks for the
same key.

## The block

```yaml
jobs:
  install:
    image: node:22
    cache:
      - key: pnpm-store-${CI_COMMIT_BRANCH}
        paths:
          - .pnpm-store
          - node_modules
    script:
      - pnpm install --frozen-lockfile
```

Two parts:

- `key:` — a templated string. Two runs that resolve to the same
  key share the cache.
- `paths:` — directories OR files to tar. Glob patterns supported.

## Key templating

The string is expanded at dispatch time:

| Variable | Expands to |
|---|---|
| `${CI_COMMIT_BRANCH}` | branch name (sanitised, slashes → `-`) |
| `${CI_COMMIT_SHORT_SHA}` | first 8 chars of the SHA |
| `${CI_PIPELINE_NAME}` | pipeline name |
| `${CI_PROJECT_SLUG}` | project slug |
| `${CI_JOB_NAME}` | the current job's name |
| `${CI_RUN_COUNTER}` | run number for this pipeline |

Common patterns:

```yaml
# Per-branch (recommended for most caches): main and feature
# branches keep separate buckets, no cross-poisoning.
key: pnpm-${CI_COMMIT_BRANCH}

# Per-job, per-branch (when different jobs need different cache
# contents from the same install).
key: ${CI_JOB_NAME}-${CI_COMMIT_BRANCH}

# Lockfile-keyed (when you want every lockfile change to start
# clean — costly but airtight reproducibility):
key: pnpm-${HASH:pnpm-lock.yaml}    # hash-of-file syntax (planned)

# Per-pipeline (when one cache works across all jobs):
key: shared-${CI_PIPELINE_NAME}-${CI_COMMIT_BRANCH}
```

`${HASH:path}` for content-keyed caches isn't shipped yet — for
now, when lockfile semantics matter, restart the cache by bumping
the suffix manually (`pnpm-store-v2`, `pnpm-store-v3`). The retention
sweeper drops orphaned entries automatically.

## What to cache (by toolchain)

### Node / pnpm

```yaml
cache:
  - key: pnpm-${CI_COMMIT_BRANCH}
    paths: [.pnpm-store, node_modules]
```

Plugin sets `pnpm config store-dir /workspace/.pnpm-store` so the
content-addressable store lives in the workspace. `node_modules/`
also caches the resolved tree so install is just a verify pass.

### Go

```yaml
cache:
  - key: go-${CI_COMMIT_BRANCH}
    paths: [.go-mod, .go-cache]
```

Plugin redirects `GOMODCACHE=/workspace/.go-mod` and
`GOCACHE=/workspace/.go-cache`. Both matter — `.go-mod` is fetched
modules, `.go-cache` is compiled package artefacts (incremental
builds + memoised test results).

### Maven

```yaml
cache:
  - key: maven-${CI_COMMIT_BRANCH}
    paths: [.m2]
```

Plugin redirects the local repository to `/workspace/.m2`. A
typical 200-dep project lands at 200-500 MB.

### Gradle

```yaml
cache:
  - key: gradle-${CI_COMMIT_BRANCH}
    paths: [.gradle-user-home, .gradle-cache]
```

Two paths because Gradle has TWO independent caches:
`GRADLE_USER_HOME` (deps) and the build cache (`--build-cache`,
task-output memo). Cache both for full warmth.

### Python (uv)

```yaml
cache:
  - key: uv-${CI_COMMIT_BRANCH}
    paths: [.uv-cache, .venv]
```

uv writes its package cache to `.uv-cache` when run from
`/workspace` (default). The resolved venv at `.venv/` should also
travel — restoring it skips the resolver.

### Python (Poetry)

```yaml
cache:
  - key: poetry-${CI_COMMIT_BRANCH}
    paths: [.cache/pypoetry, .venv]
```

Plugin sets `POETRY_VIRTUALENVS_IN_PROJECT=1` so `.venv` is local
and `POETRY_CACHE_DIR=/workspace/.cache/pypoetry` so the wheel
cache is in the workspace.

### Docker buildx

```yaml
# buildx caches go through buildx's own cache backends, not the
# platform's `cache:` block. Use cache_from / cache_to in the
# plugin's inputs:
build:
  uses: gocdnext/buildx@v1
  with:
    cache_from: type=gha,scope=myapp
    cache_to: type=gha,scope=myapp,mode=max
```

The `cache:` block is for filesystem dirs; buildx's layer cache is
content-addressable in the registry, which the platform doesn't
mediate.

## Eviction

Caches age out. The retention sweeper runs every tick and:

1. Drops entries whose `last_accessed_at` is past the configured
   TTL (default 30 days, `caches.ttlDays` in Helm).
2. Drops oldest entries when a project exceeds its quota
   (`caches.projectQuotaBytes`).
3. Drops oldest entries globally when the total exceeds the
   global quota (`caches.globalQuotaBytes`).

What "oldest" means: LRU on `last_accessed_at`. Caches that get
hit on every push survive forever; abandoned project caches age
out within a month.

`last_accessed_at` is updated whenever a job restores from the
cache, so an active project on `feature/foo` keeps that branch's
cache warm even if main hasn't touched it in weeks.

## Pre-warming + invalidation

### Pre-warm a feature branch

When you create a feature branch, the first run on it hits a
cold cache (no entry under `pnpm-feature-foo`). To pre-warm,
manually trigger a run on the new branch — subsequent runs hit
the warmed cache.

For long-lived feature branches with frequent CI, this matters
less than you'd think; the cache warms within 1-2 runs.

### Invalidate when something changes the resolution

When you bump a Node major version, the lockfile changes, the
resolved tree changes, but `pnpm-${CI_COMMIT_BRANCH}` still
points at the old cache → install fails or behaves weirdly.
Force-invalidate by bumping the key:

```yaml
key: pnpm-v2-${CI_COMMIT_BRANCH}    # was pnpm-${CI_COMMIT_BRANCH}
```

The retention sweeper drops the old `pnpm-*` entries on its
quota pass.

## Common pitfalls

- **Caching the wrong path**: Maven's `.m2/` outside `/workspace`
  is the system default — agents can't see it. The plugins
  REDIRECT into the workspace; if you bypass the plugin (use
  `image: maven:3.9` + `script:` directly), set `MAVEN_OPTS=
  -Dmaven.repo.local=/workspace/.m2` yourself.
- **Cache size growth**: Gradle's build cache can hit 5+ GiB on
  big multi-module projects. Set `caches.projectQuotaBytes` in
  Helm to bound; LRU eviction trims when needed.
- **Cache corruption**: a flaky agent can write a partial tar.
  The cache restore pass treats CRC failures as "miss" and
  proceeds to a cold install. The corrupt entry stays around
  until quota / TTL evicts it. Symptom: random "missing
  dependency" errors that go away on rerun.
- **Sharing cache across pipelines**: caches are scoped per
  project, not per pipeline. If pipeline A and pipeline B in
  the same project both have `key: pnpm-main`, they share the
  cache. Use `${CI_PIPELINE_NAME}` in the key to scope tighter.
