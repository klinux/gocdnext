---
title: Upgrade runbook
description: Move between gocdnext versions safely — what to back up, how migrations land, and how to roll back when something goes sideways.
---

gocdnext follows semver loosely: minor bumps (`0.5.x → 0.6.0`)
land database migrations + new features but stay backward-
compatible at the YAML/API surface. Every release tag has notes
flagging breaking changes specifically.

This page walks the canonical Helm upgrade. Adapt for your own
deployment automation (Argo CD, Flux, Pulumi, …) — the steps don't
change, only how `helm upgrade` is invoked.

:::caution[v0.5.0 BREAKING DEFAULT — Kubernetes workspace]
The `agent.workspace.accessMode` default flipped from `ReadWriteMany`
to `ReadWriteOnce`. Every job pod now gets its own ephemeral PVC
(see [Kubernetes runtime](/gocdnext/docs/concepts/kubernetes-runtime/)).

If you were on v0.4.x with `agent.engine: kubernetes` and an RWX
storage class (Filestore, NFS), you have two choices:

1. **Keep legacy** — pin `agent.workspace.accessMode: ReadWriteMany`
   in your values file BEFORE running `helm upgrade`. Behaviour is
   identical to v0.4.x.
2. **Migrate to isolated** — set
   `agent.workspace.storageClassName: <a-RWO-class>` (e.g. `pd-ssd`,
   `local-ssd`, `gp3`). Drop NFS/Filestore. Faster, simpler, but
   jobs no longer share a workspace between each other.

If you run `helm upgrade` without setting either, the default
flips and `volume.ephemeral` will try to provision against the
cluster's default storage class, which may or may not be RWO.
:::

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
  --version 0.6.4 \
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

- Lists new migration numbers in the release notes (e.g. `00036`,
  `00039`).
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
goose: applied 00036_runs_has_services.sql in 12ms
goose: applied 00037_agents_engine.sql in 4ms
goose: applied 00038_idx_job_runs_run_id.sql in 41ms
goose: applied 00039_service_runs.sql in 22ms
goose: successfully migrated database to version: 39
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
