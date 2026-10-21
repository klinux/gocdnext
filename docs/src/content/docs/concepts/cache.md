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

You rarely need per-branch *keys*, because writes are already
isolated per ref — see below.

## Branch isolation (PR-safe scoping)

Even when every branch resolves to the **same key**, their cache
writes never clobber each other. The server scopes each write to a
**ref lane**, GitHub-Actions style — automatically, with nothing to
configure in the pipeline:

- A push / integration to the project **default branch** writes the
  shared lane (`@`).
- Every other ref — feature-branch push, pull request, merge queue,
  tag — writes its **own** isolated lane and **reads** its own lane
  first, then falls back to the shared lane as a base.

So a pull request restores the default branch's cache (warm start),
then saves its own additions to a private lane. A second PR running
concurrently can't overwrite the first's cache or the default
branch's — the clobbering that produced "cache hit but still a
dependency miss" under a single shared blob is gone.

Why this matters for **additive** caches (Gradle's build cache, in
particular): the blob is replaced whole on each write, but task
outputs accumulate. Before scoping, two PRs restoring the same key,
each adding their own entries, raced — the last writer's blob
overwrote the others, so the next run missed entries that *were*
computed. Per-lane writes remove the race: each ref owns its blob.

This is a server policy; pipelines don't opt in and there's no YAML
field. Operators see the lane per row under *Project → Caches*
("default" = the shared lane).

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

For the **build cache** specifically there's a better option than
tarring the whole directory — see
[Embedded Gradle build cache](#embedded-gradle-build-cache) below.
Keep the `cache:` block for `GRADLE_USER_HOME` (deps); point
`--build-cache` at the embedded remote cache.

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

## Embedded Gradle build cache

The `cache:` block tars a whole directory and stores it under one
key. That's the right model for a dependency store (pnpm, `.m2`,
`GRADLE_USER_HOME`), but it's a mismatch for **Gradle's build
cache**, which is *entry-addressable*: each task output is keyed by
a hash of its inputs. gocdnext can serve Gradle's build cache
natively — it implements the
[Gradle HTTP build cache protocol](https://docs.gradle.org/current/userguide/build_cache.html#sec:build_cache_configure_remote),
so each task output is one object keyed by its hash.

Why this is better than tarring the build-cache dir:

- **Entry-level sharing.** Two branches/PRs that compute the same
  task output share the same entry (same input hash → same key).
- **No clobbering, ever.** Entries are immutable and idempotent —
  concurrent writers of the same key write identical bytes.
- **No whole-dir transfer.** A build fetches only the entries it
  needs and pushes only new ones.
- **No extra service.** It's an endpoint on the gocdnext server,
  backed by the same object store. The handler is stateless, so scale
  it with server replicas. (Entry bytes are proxied through the server:
  Gradle's cache client rejects HTTP redirects, so a signed-URL
  redirect isn't an option.)

### Enable it

In **Project → Settings → Gradle build cache** (or
`POST /api/v1/projects/{slug}/build-cache/token`), click **Enable**.
That generates a project token (shown **once**; only its hash is
stored).

**Jobs that use the [`gradle` plugin](/gocdnext/docs/) are then wired
automatically** — the server injects the cache URL + a short-lived,
per-run token into the job's env (the token is masked in logs), and the
plugin drops a Gradle init script that points `HttpBuildCache` at
gocdnext. Nothing to add to your repo; just run the build.

Auto-wiring needs the operator to set a signing key on the server
(`GOCDNEXT_BUILD_CACHE_SIGN_KEY`, hex) — without it, the manual path
below still works.

### Manual setup (non-plugin builds)

For a build that doesn't use the `gradle` plugin, point Gradle at the
cache yourself in `settings.gradle.kts` (the rotate response gives you
this snippet pre-filled).

:::caution
The **static project token is read/write and has no ref awareness** —
anyone who can present it can push. The read-only-for-PRs guarantee
only applies to the auto-wired (`gradle`-plugin) path, where the server
mints a per-run token scoped to the run's ref. If you use the static
token, give it **only to trusted (default-branch) builds** — store it
as a secret that PR jobs can't read. Prefer the auto-wired path when
you can.
:::

```kotlin
buildCache {
    remote<HttpBuildCache> {
        url = uri("https://<your-gocdnext>/api/v1/buildcache/<project-slug>/")
        isPush = true  // Kotlin DSL uses `isPush`; Groovy uses `push`
        credentials {
            username = "gocdnext"
            password = System.getenv("GOCDNEXT_CACHE_TOKEN") // the generated token
        }
    }
}
```

Run with `--build-cache` (or `org.gradle.caching=true`). Keep the
`cache:` block for `GRADLE_USER_HOME` (dependencies); the embedded
cache handles the task-output build cache.

The token authenticates the build (HTTP Basic) and scopes it to one
project — a token for project A can't read or write project B's
entries. Rotating or disabling the token is immediate (a disable
revokes outstanding auto-wired tokens too).

**Only default-branch builds write; PRs and feature branches are
read-only.** Auto-wired PR/branch runs get a read-only token (`push`
off), so an untrusted run can read the default branch's entries but
can't poison the shared cache or exhaust its storage — the same
trust model as [PR-safe scoping](#branch-isolation-pr-safe-scoping).
A single entry PUT is capped (`GOCDNEXT_BUILD_CACHE_MAX_ENTRY_MB`,
default 512). Entry bytes are proxied through the server (Gradle's
cache client doesn't follow redirects); the handler is stateless, so
scale reads with server replicas.

### Eviction

Entries are immutable objects in the bucket. For now, bound growth
with an **object-store lifecycle rule** on the `gradle-cache/`
prefix (e.g. "delete objects older than N days") — a content-
addressed cache tolerates this: a deleted entry is just recomputed.
Server-tracked, last-access-based eviction is planned.

## Eviction

Caches age out. The retention sweeper runs every tick and:

1. Drops entries whose `last_accessed_at` is past the configured
   TTL. The **shared** (default-branch) lane uses the long horizon
   (default 30 days, `caches.ttlDays` in Helm); the **isolated**
   per-ref lanes use a shorter one (default 7 days,
   `caches.scopedTtlDays`) so a merged or abandoned branch's cache
   surrenders disk fast instead of multiplying with branch churn. A
   PR reading the shared lane bumps *its* freshness, so the default
   branch's cache never ages out from under active PRs.
2. Drops oldest entries when a project exceeds its quota
   (`caches.projectQuotaBytes`).
3. Drops oldest entries globally when the total exceeds the
   global quota (`caches.globalQuotaBytes`).
4. Reclaims rows stuck in `pending` past a grace window (an agent
   that requested an upload slot and died before finalising it).
   Per-ref lanes make these accumulate with branch churn, so the
   sweep bounds the leak.

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
  The cache restore pass treats a failed restore as a miss, wipes
  the partially-extracted paths, and proceeds to a cold install.
  The corrupt entry stays around until quota / TTL evicts it.
  Symptom: random "missing dependency" errors that disappear on the
  next run — clear the cache via the dashboard if you suspect this.
- **"Cache hit but still a dependency miss" across concurrent PRs**:
  was caused by branches clobbering one shared blob. Fixed by
  [PR-safe scoping](#branch-isolation-pr-safe-scoping) — each ref
  writes its own lane now. If you still see it, confirm the task's
  inputs aren't PR-specific (e.g. a per-build `-Drevision` baked into
  a compile task's hash poisons the build-cache key itself).
- **Shell-style vars in `key:`**: `${CI_BRANCH}` (and friends)
  stay LITERAL in cache keys — they don't expand. Use `{{ hash }}`
  for content-keyed caches; use constant strings otherwise.
