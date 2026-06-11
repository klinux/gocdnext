---
title: Database migrations in pipelines
description: "Expand/contract, canary/blue-green safety rules, lock hygiene and job patterns for the flyway / liquibase / goose plugins."
---

Schema migrations are the coupling point between your deploy
strategy and your database. A canary or blue-green rollout means
**two versions of the application run simultaneously against the
same schema** — so there is no such thing as a "canary migration";
there are migrations that are *safe under two live versions* and
migrations that aren't. This page is the contract for writing the
first kind, plus the pipeline patterns for the
[flyway](/reference/plugins/#flyway),
[liquibase](/reference/plugins/#liquibase) and
[goose](/reference/plugins/#goose) plugins.

## The one rule: expand/contract

Every schema change ships in (at least) two releases:

1. **Expand** (release N): make the schema a superset that serves
   both versions. Add the new column (nullable or with a default),
   add the new table, start dual-writing. Old code keeps working
   untouched; new code uses the new shape.
2. **Contract** (release N+1 or later): once no running version
   reads the old shape — confirmed, not assumed — drop the old
   column/table in a separate migration.

The corollary is the deploy ordering:

```
migrate (schema N+1) ──▶ deploy canary (app N+1) ──▶ promote ──▶ [later release] contract
```

The migration runs **before** the rollout starts, never in the
middle of it. Schema N+1 must serve app N (still 95% of traffic)
and app N+1 (the canary) at the same time. That's the whole test
for whether your migration is canary-safe: *would this change
break the version currently running in production?*

## What breaks two live versions (the prohibition table)

| Change | Why it breaks canary/BG | Safe alternative |
|---|---|---|
| Rename a column/table in one step | Old version reads the old name → instant 500s on 95% of traffic | Expand: add new name, dual-write, backfill; contract: drop old name later |
| Change a column type in place | Rewrite locks the table AND old code may not parse the new type | New column of the new type, dual-write, swap reads, drop old |
| `ADD COLUMN ... NOT NULL` without default | Old version's INSERTs don't supply it → constraint violations | Add nullable (or with `DEFAULT`), backfill, then add the constraint in a later migration |
| Drop a column/table the running version still reads | Old version queries it → errors until the rollout finishes (or aborts!) | Contract only after the last reader is gone — a release boundary, not a deploy boundary |
| Tighten a constraint old data/code violates | Old writes start failing mid-rollout | Validate data first; add constraint `NOT VALID` then `VALIDATE CONSTRAINT` separately |

A canary that aborts is the stress test for these rules: traffic
shifts BACK to the old version. If your migration only served the
new code, the rollback of the *app* now runs against a schema the
old code can't use — and you're fixing prod at 3am. Expand/contract
makes app rollback always safe because the schema is always a
superset of both versions' needs.

## Forward-only: rollback is a new migration

None of the three plugins expose `down` / `rollback` / `repair`
commands, deliberately. Down-migrations in production combine the
worst properties: they're rarely tested, they run against data the
up-migration already transformed, and after an aborted canary they
race the app rollback. The contract instead:

- **Broken migration applied?** Ship a corrective *forward*
  migration. The history table stays append-only and every
  environment converges by replaying the same sequence.
- **`flyway repair` / history surgery**: operator action with
  human context, run manually — never from a pipeline retry loop.

This is the same policy gocdnext applies to its own schema (goose,
forward-only, no `.down.sql` in production), and the same
prerequisite the
[trunk-based release guide](/concepts/trunk-based-release/#3-migrations-are-forward-only-and-backward-compatible-expandcontract)
treats as a hard requirement for frequent deploys.

## Lock hygiene (Postgres)

The classic outage: a "tiny" `ALTER TABLE` waits for a lock behind
a long transaction — and every query issued after it queues behind
the ALTER. The site is down because of a migration that hasn't
even run yet. Two session settings prevent it:

- `lock_timeout` — the DDL fails fast (retry later) instead of
  queueing the world behind it. The flyway plugin injects
  **5s by default** via initSql; for liquibase/goose put it in the
  connection string (see each plugin's examples):
  `...?options=-c%20lock_timeout%3D5s`
- `statement_timeout` — caps a runaway backfill or accidental
  table rewrite (flyway plugin default: 15min).

Also Postgres-specific:

- `CREATE INDEX CONCURRENTLY` for any index on a busy table — a
  plain CREATE INDEX takes a lock that blocks writes for the whole
  build. Caveat: CONCURRENTLY can't run inside a transaction —
  flyway needs `V3__x.sql` with a non-transactional config
  (`executeInTransaction=false` in a config-file entry), goose
  needs `-- +goose NO TRANSACTION`.
- Big backfills belong in batched application code or a dedicated
  migration with its own raised `statement-timeout`, not in the
  same migration as the DDL.

## Pipeline patterns

### Validate on PR, migrate on main — as TWO pipelines

The branch gate is **structural, not conditional**: the mutating
pipeline's material listens to `push` only, so a pull-request run
of it simply never exists. Don't reach for `rules:` as the safety
rail here — `rules` is parsed but not enforced at dispatch today
(see the [YAML reference](/pipelines/yaml-reference/)), and a
guard that doesn't guard is worse than none.

```yaml
# .gocdnext/db-verify.yaml — PRs and pushes; ZERO mutating
# commands anywhere in this pipeline.
name: db-verify
materials:
  - git:
      url: https://github.com/acme-org/shop
      branch: main
      on: [push, pull_request]
stages: [check]

jobs:
  validate:
    stage: check
    secrets: [FLYWAY_URL, FLYWAY_USER, FLYWAY_PASSWORD]
    uses: ghcr.io/klinux/gocdnext-plugin-flyway@v1
    with:
      command: validate
```

```yaml
# .gocdnext/db-migrate.yaml — push to main ONLY (no pull_request
# in `on:` → no PR run can ever dispatch the mutating command),
# human-approved, BEFORE any deploy that needs the new schema.
name: db-migrate
materials:
  - git:
      url: https://github.com/acme-org/shop
      branch: main
      on: [push]
stages: [migrate]

jobs:
  approve:
    stage: migrate
    approval:
      approvers: [dba, platform-lead]

  migrate:
    stage: migrate
    needs: [approve]
    secrets: [FLYWAY_URL, FLYWAY_USER, FLYWAY_PASSWORD]
    uses: ghcr.io/klinux/gocdnext-plugin-flyway@v1
    with:
      command: migrate
```

### Migration before canary (the ordering as a DAG)

```yaml
jobs:
  # Gate #1: the schema apply itself — same rule as every
  # mutating command in this page's checklist.
  approve-migration:
    stage: release
    approval:
      approvers: [dba, platform-lead]

  migrate:
    stage: release
    needs: [approve-migration]
    secrets: [GOOSE_DBSTRING]
    uses: ghcr.io/klinux/gocdnext-plugin-goose@v1
    with: { command: up, dir: server/migrations }

  deploy-canary:
    stage: release
    needs: [migrate]          # schema N+1 in place FIRST
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      command: upgrade --install app ./chart --set image.tag=${CI_COMMIT_SHORT_SHA}

  # Gate #2: canary promotion — while this waits, app N and app
  # N+1 are BOTH live against schema N+1. Expand/contract is what
  # makes that window safe.
  approve-promote:
    stage: promote
    needs: [deploy-canary]
    approval:
      approvers: [oncall]
```

### Credentials

Connection strings and passwords go through the job's `secrets:`
list, never through `with:` inputs — `with:` values are part of
the persisted pipeline definition. Each plugin reads its tool's
native env (`FLYWAY_*`, `LIQUIBASE_COMMAND_*`, `GOOSE_DBSTRING`),
so the values also never appear on argv. The agent masks secret
values in job logs.

## Checklist before merging a migration

- [ ] Serves BOTH the running version and the next one (expand)
- [ ] No rename / in-place type change / unqualified NOT NULL / drop-still-read
- [ ] Contract steps split into a later release
- [ ] Index creation uses CONCURRENTLY (and non-transactional mode)
- [ ] Lock/statement timeouts in place (plugin default or DSN options)
- [ ] PR pipeline ran `validate` (+ `update-sql`/`info` preview)
- [ ] `migrate`/`update`/`up` is gated: protected branch + approval
- [ ] Rollback story = corrective forward migration (no down in prod)
