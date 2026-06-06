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
      - key: pnpm-store-{{ hash "pnpm-lock.yaml" }}
        paths:
          - .pnpm-store
          - node_modules
    script:
      - pnpm install --frozen-lockfile
```

Two parts:

- `key:` — the cache name. Two runs with the same resolved key
  share the cache entry.
- `paths:` — directories OR files to tar. Glob patterns
  supported.

## Key templating

There is **one** template token in the cache key grammar:

| Token | Expansion |
|---|---|
| `{{ hash "<glob>" }}` | hex digest of the sorted files matching the glob (workspace-relative). |

The agent's cache resolver expands `{{ hash }}` tokens before
fetching/storing — content-keyed caches "just work":

```yaml
cache:
  - key: pnpm-store-{{ hash "pnpm-lock.yaml" }}
    paths: [.pnpm-store]
```

Bump the lockfile → digest changes → new cache bucket. Older
buckets age out via the retention sweeper.

The glob is workspace-relative with no `..` traversal. A glob
that matches zero files aborts the job rather than silently
producing an empty hash. Limits: ≤ 100 files matched, ≤ 16 MiB
per file, ≤ 64 MiB total — set generously to make the audit
trail predictable.

### Shell-style `${VAR}` is NOT expanded in cache keys

Cache keys are passed to the storage backend verbatim. The
agent does not run shell-style variable expansion on them
(`${CI_BRANCH}`, `${CI_PIPELINE_NAME}`, etc. stay as **literal
text**). So this:

```yaml
key: pnpm-store-${CI_BRANCH}
```

becomes the literal storage key `pnpm-store-${CI_BRANCH}` — every
branch shares the same bucket. That's fine for most caches (pnpm
store, Go mod cache, Maven .m2) because the contents are
content-addressable and multi-version-safe; cohabitation is the
whole design.

When you genuinely need per-branch isolation, use a different
constant per pipeline (`ci-server-cache`, `ci-web-cache`) or use
`{{ hash }}` against a file that changes per branch (rare). For
the common "invalidate on lockfile change" case, hash-keyed is
the right tool.

## What to cache (by toolchain)

### Node / pnpm

```yaml
cache:
  - key: pnpm-store-{{ hash "pnpm-lock.yaml" }}
    paths: [.pnpm-store, node_modules]
```

Plugin sets `pnpm config store-dir /workspace/.pnpm-store` so the
content-addressable store lives in the workspace. `node_modules/`
also caches the resolved tree so install is just a verify pass.

### Go

```yaml
cache:
  - key: go-{{ hash "go.sum" }}
    paths: [.go-mod, .go-cache]
```

Plugin redirects `GOMODCACHE=/workspace/.go-mod` and
`GOCACHE=/workspace/.go-cache`. Both matter — `.go-mod` is fetched
modules, `.go-cache` is compiled package artefacts (incremental
builds + memoised test results).

### Maven

```yaml
cache:
  - key: maven-{{ hash "pom.xml" }}
    paths: [.m2]
```

Plugin redirects the local repository to `/workspace/.m2`. A
typical 200-dep project lands at 200-500 MB.

### Gradle

```yaml
cache:
  - key: gradle-{{ hash "**/build.gradle.kts" }}
    paths: [.gradle-user-home, .gradle-cache]
```

Two paths because Gradle has TWO independent caches:
`GRADLE_USER_HOME` (deps) and the build cache (`--build-cache`,
task-output memo). Cache both for full warmth.

### Python (uv)

```yaml
cache:
  - key: uv-{{ hash "uv.lock" }}
    paths: [.cache, .venv]
```

uv writes its package cache to `.cache/uv/` when run from
`/workspace`. The resolved venv at `.venv/` should also travel —
restoring it skips the resolver.

### Python (Poetry)

```yaml
cache:
  - key: poetry-{{ hash "poetry.lock" }}
    paths: [.cache, .venv]
```

Plugin sets `POETRY_VIRTUALENVS_IN_PROJECT=1` so `.venv` is local
and `POETRY_CACHE_DIR=/workspace/.cache/pypoetry` so the wheel
cache is in the workspace.

### trivy (security DB)

```yaml
cache:
  - key: trivy-db
    paths: [.cache/trivy]
```

Trivy's vulnerability DB is the cached artefact, not anything
project-derived — use a constant key so all projects share the
warm DB. Pair with `skip-db-update: "true"` on air-gapped agents.

### Docker buildx

buildx layer caches go through buildx's own cache backends, not
the platform's `cache:` block. Use the plugin's `cache:`/
`cache-from:`/`cache-to:` inputs:

```yaml
build:
  uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
  with:
    cache-from: type=gha,scope=myapp
    cache-to: type=gha,scope=myapp,mode=max
```

The platform's `cache:` block is for filesystem dirs; buildx's
layer cache is content-addressable in the registry, which the
platform doesn't mediate. See the [layer-cache recipe](/gocdnext/docs/pipelines/recipes/layer-cache/)
for the runner-profile pattern.

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
cache, so an active project on a feature branch keeps that
hash-key's cache warm even if main hasn't touched it in weeks.

Operators can also purge caches manually via *Project → Caches*
in the dashboard.

## Pre-warming + invalidation

### Pre-warm

When you bump a lockfile, the first run with the new hash key
hits a cold cache. That's expected — the cost amortises over
subsequent runs against the same hash.

For projects with very long install times, pre-warm by triggering
a *Run latest* on the new lockfile state ahead of merging the
bump PR.

### Invalidate when something changes the resolution

`{{ hash "lockfile" }}` invalidates automatically. For caches
keyed on a constant string, force-invalidate by bumping the key:

```yaml
key: pnpm-store-v2     # was pnpm-store
```

The retention sweeper drops the old `pnpm-store` entries on its
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
  dependency" errors that disappear on the next run — clear
  the cache via the dashboard if you suspect this.
- **Shell-style vars in `key:`**: `${CI_BRANCH}` (and friends)
  stay LITERAL in cache keys — they don't expand. Use `{{ hash }}`
  for content-keyed caches; use constant strings otherwise.
