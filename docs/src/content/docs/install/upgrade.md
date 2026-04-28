---
title: Upgrade runbook
description: Move between gocdnext versions safely — what to back up, how migrations land, and how to roll back when something goes sideways.
---

gocdnext follows semver loosely: minor bumps (`0.1.x → 0.2.0`)
land database migrations + new features but stay backward-
compatible at the YAML/API surface. Every release tag has notes
flagging breaking changes specifically.

This page walks the canonical Helm upgrade. Adapt for your own
deployment automation (Argo CD, Flux, Pulumi, …) — the steps don't
change, only how `helm upgrade` is invoked.

## Before you upgrade

### 1. Read the release notes

Every tag at <https://github.com/klinux/gocdnext/releases> has the
diff vs the previous tag, schema migrations listed by number, and
any breaking changes called out at the top. Skim before you pull
the trigger — there's usually nothing urgent, but knowing what's
about to change makes triage easier if a job behaves differently.

### 2. Back up the database

The control plane is **stateful** — pipelines, runs, log_lines,
artefacts metadata, secrets — all live in Postgres. The chart
doesn't run automatic backups; that's your operator's call (Velero,
pgBackRest, native logical dumps, whatever your platform team uses).

Quick logical dump for emergencies:

```bash
kubectl -n gocdnext exec -it gocdnext-postgres-dev-0 -- \
  pg_dump -U gocdnext -F c -d gocdnext -f /tmp/gocdnext-pre-upgrade.dump
kubectl -n gocdnext cp gocdnext-postgres-dev-0:/tmp/gocdnext-pre-upgrade.dump \
  ./gocdnext-pre-upgrade.dump
```

For external Postgres replace with whatever your standard backup
flow is. Restore is `pg_restore -d gocdnext gocdnext-pre-upgrade.dump`.

### 3. Pause the agents (optional)

A clean upgrade doesn't require this — agents reconnect after the
control plane rolls. But if you want to avoid jobs landing
mid-rollout (and possibly being marked `running` against an agent
the new control plane doesn't know about yet), scale them to zero
first:

```bash
kubectl -n gocdnext scale deployment/gocdnext-agent --replicas=0
```

The reaper picks up any stale `running` rows after the upgrade
and re-queues them automatically — the timing window where this
matters is small.

## The upgrade

```bash
helm upgrade gocdnext oci://ghcr.io/klinux/charts/gocdnext \
  --version 0.2.0 \
  --namespace gocdnext \
  --reuse-values
```

`--reuse-values` keeps every override you set on previous installs.
If you want to change something at the same time, pass `--set` or
`-f values-prod.yaml` instead.

What this does:

1. Pulls the chart from the OCI registry.
2. Rolls the control plane Deployment first. The new pod boots,
   runs goose migrations on the database, then accepts traffic.
   Old pod doesn't terminate until the new pod's `readinessProbe`
   passes — readiness is gated on the migration completing.
3. Rolls the agent Deployment(s). New agents register, old ones
   close their gRPC stream. The session store invalidates the old
   ones.
4. Rolls the web Deployment.

## Migration ordering

Migrations run **forward-only** (no `.down.sql` in production —
the project doctrine). Each release:

- Lists new migration numbers in the release notes (e.g. `00027`,
  `00028`).
- Migrations are idempotent at goose's level — `goose up` is safe
  to re-run.
- Migrations are designed to be backward-compatible with the
  previous server version. The new server can drive the new
  schema; the old server doesn't BREAK on the new schema either,
  because we never drop columns or rename without a deprecation
  cycle.

That posture means a rollback (downgrade the server image to the
previous version) is generally safe: the old binary keeps running
against the new schema. Specific guidance lives in the release
notes — read those before downgrading.

## After the upgrade

### 1. Verify the rollouts

```bash
kubectl -n gocdnext get pods
kubectl -n gocdnext rollout status deployment/gocdnext-server
kubectl -n gocdnext rollout status deployment/gocdnext-agent
kubectl -n gocdnext rollout status deployment/gocdnext-web
```

All `Running` + `Ready`. The server's `/healthz` returns 200; if
not, check the logs:

```bash
kubectl -n gocdnext logs deployment/gocdnext-server --tail=200
```

### 2. Check the migration trail

Boot logs from the new server show every migration that ran:

```
goose: applied 00027_log_lines_partition.sql in 38ms
goose: applied 00028_log_archive.sql in 1ms
goose: successfully migrated database to version: 28
```

If you see fewer migrations than the release notes mentioned, the
DB might already have been at a later state — verify via:

```sql
SELECT * FROM goose_db_version ORDER BY id DESC LIMIT 10;
```

### 3. Smoke-test a run

Trigger a pipeline you trust (the *Run latest* button on a known-
green pipeline). If it goes through, the upgrade is real.

### 4. Resume agents (if paused)

```bash
kubectl -n gocdnext scale deployment/gocdnext-agent --replicas=2
```

## Rolling back

If the new version misbehaves and rollback is the right answer:

```bash
helm rollback gocdnext --namespace gocdnext
```

This bumps every Deployment back to the previous chart's image
tags AND keeps the database at the new schema (forward-only).
Because migrations are designed to be backward-compatible with
the previous server version, the old server will run fine against
it — but verify against the release notes for the version you're
rolling back FROM, since that's where any incompatibility would
be flagged.

If a release notes specifically calls out a non-backward-compatible
migration (rare, never silent), the rollback path includes a
data-fixup query at the top — read it before running.

## Going across major versions

`0.x → 1.0` (when it lands) will likely include a one-shot
migration that's not backward-compatible. Treat it as a
maintenance window:

1. Take the database backup (above).
2. Stop the control plane (`replicas: 0`).
3. Run the migration: `kubectl run --rm -it gocdnext-migrate
   --image=ghcr.io/klinux/gocdnext-server:1.0.0 -- migrate up`.
4. Start the new server.
5. Verify, then resume agents.

This pattern is standard for major DB schema overhauls; the
release notes will say which version triggers it. For now, every
0.x release is in-place safe.
