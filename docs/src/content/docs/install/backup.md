---
title: Backup & restore
description: What state is precious, how to back it up, how to restore it.
---

gocdnext keeps three pools of state. Two are critical (lose them
and you lose history); one is rebuildable (lose it and you
re-fetch).

| State | Where | Lose-it impact | Backup priority |
|---|---|---|---|
| Postgres | `gocdnext-postgres-dev-0` (dev) or external | Everything: pipelines, runs, secrets, audit, RBAC | **Critical** |
| Artefact backend | filesystem PVC, S3, GCS | Per-run binaries + log archives | **High** |
| Plugin images | GHCR or your registry | Re-buildable from the source repo | Low |

This page walks the runbook for the first two; plugin images
are out of scope (your registry's own backup, your CI's
ability to re-publish from source).

## Postgres backup

### Logical dump (simplest)

Works for any Postgres setup — bundled `postgres-dev` or external.

```bash
# Bundled postgres-dev:
kubectl -n gocdnext exec gocdnext-postgres-dev-0 -- \
  pg_dump -U gocdnext -F c -d gocdnext > gocdnext-$(date +%Y%m%d).dump

# External Postgres (run from anywhere with network access):
PGPASSWORD=... pg_dump -h db.internal -U gocdnext -F c -d gocdnext \
  > gocdnext-$(date +%Y%m%d).dump
```

The `-F c` (custom format) is what `pg_restore` consumes —
slightly more efficient than plain SQL and supports parallel
restore.

A nightly cron in your platform (Velero schedule, K8s CronJob,
external job) is the typical setup. Retain 30 days; weekly +
monthly slot rotation if you want to go further back.

### Physical backup (large deployments)

When the database is big enough that logical dumps take hours,
use Postgres' physical replication or PITR via `pg_basebackup`
+ archived WAL. [pgBackRest](https://pgbackrest.org/) and
[Barman](https://pgbarman.org/) automate this. Both write to
S3-compatible storage, support point-in-time recovery, and
work transparently with Postgres operators (Crunchy, Zalando,
CloudNativePG).

This is operator-level work — not specific to gocdnext.

### Restore

```bash
# Drop the existing database (DESTRUCTIVE — make sure backup is recent):
PGPASSWORD=... psql -h db.internal -U postgres -d postgres \
  -c "DROP DATABASE gocdnext;"
PGPASSWORD=... psql -h db.internal -U postgres -d postgres \
  -c "CREATE DATABASE gocdnext OWNER gocdnext;"

# Restore:
PGPASSWORD=... pg_restore -h db.internal -U gocdnext \
  -d gocdnext --no-owner --no-acl gocdnext-2026-04-28.dump
```

After restore, restart the gocdnext server. It re-runs goose
against the restored schema — every migration listed in
`goose_db_version` already applied is a no-op.

## Artefact backend backup

### Filesystem backend (PVC)

Use whatever your platform offers for PVC snapshots:

- **CSI snapshot** (most modern CSI drivers) — instant, atomic.
  Standard Kubernetes resource:

  ```yaml
  apiVersion: snapshot.storage.k8s.io/v1
  kind: VolumeSnapshot
  metadata:
    name: gocdnext-artifacts-2026-04-28
    namespace: gocdnext
  spec:
    volumeSnapshotClassName: csi-snapshot
    source:
      persistentVolumeClaimName: gocdnext-server-artifacts
  ```

- **Velero** with the corresponding restic / Kopia / file-system
  backup plugin — works on PVCs that don't support CSI snapshots,
  trades atomicity for portability.

### S3 / GCS backend

Cloud-native — use the bucket's native versioning + lifecycle
rules:

- **S3**: enable bucket versioning + a 30-day retention rule.
  Restoration is `aws s3 sync` from a versioned read.
- **GCS**: enable Object Versioning + a noncurrent-time retention
  rule.
- **MinIO**: enable bucket versioning + ILM rules.

For cross-region disaster recovery, set up replication
(S3 Replication, GCS multi-region buckets).

### Important: artefact metadata lives in Postgres

The artefact backend has the BYTES; Postgres has the metadata
(which run produced what file, the SHA-256, the retention
stamp). Restoring one without the other gets you orphan data.

The runbook for "lost the bucket but kept Postgres":
1. Restore Postgres.
2. Bring up the server pointed at an EMPTY bucket.
3. Affected runs show `download URL` returning 404 — the file
   was uploaded but doesn't exist anymore.
4. New runs work normally.
5. The retention sweeper eventually GC's the orphan rows
   (configurable; default 30d).

The runbook for "lost Postgres but kept the bucket":
1. Restore Postgres from the dump (above).
2. Server starts. Existing runs in the dump know their
   artefact paths — the bucket has the files, downloads work.
3. Files in the bucket NOT referenced by Postgres are orphans.
   Either run `gocdnext admin orphan-cleanup --bucket <name>`
   (CLI command on the roadmap) or wait for natural retention.

### Combined "everything is gone" disaster

The platform is stateful — there's no "rebuild from source"
option for the data. Treat Postgres + the artefact bucket as
the single backup boundary; coordinate snapshots so they
correspond to the same point in time (a brief read-only window
on the platform during snapshot helps).

## Secrets

Project secrets are stored encrypted in the `secrets` table
(when `secrets.backend=db`). The decryption key is
`GOCDNEXT_SECRET_KEY` — the value Helm wires from a Kubernetes
Secret.

**Lose `GOCDNEXT_SECRET_KEY` = lose every secret.** No recovery,
the encrypted data is genuinely opaque.

Back the key up separately from the database. Common patterns:

- A sealed-secret in a separate cluster.
- A Vault that's backed up independently.
- A printout in a safe (yes, really, for the ultimate last-line).

Rotate the key on a schedule + after any suspected compromise.
The rotation flow is currently a maintenance window (re-encrypt
the secrets table); a built-in rotation tool is on the roadmap.

When `secrets.backend=kubernetes`, the secrets ARE Kubernetes
Secret objects — your existing K8s backup strategy (Velero, etcd
snapshots, sealed-secrets in Git) covers them. `GOCDNEXT_SECRET_KEY`
is irrelevant in that mode.

## Verification

A backup you haven't restored is a backup that won't work.
Quarterly verification:

1. Spin up a fresh staging gocdnext deployment in a new namespace.
2. Restore the latest production backup into it (Postgres + a
   point-in-time snapshot of the artefact bucket).
3. Verify: dashboard loads, recent runs are visible, a known
   run's logs are readable.
4. Tear down the staging deployment.

Cycle time: ~30 min for a typical mid-size install. Worth it.
The first time you actually need a restore is the worst time
to discover the dump is corrupted.
