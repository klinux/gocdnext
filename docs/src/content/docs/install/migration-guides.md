---
title: Version migration guides
description: Per-version upgrade notes — breaking changes, manual steps, rollback semantics.
---

gocdnext's release posture: **forward-only migrations, backward-
compatible binaries** within a minor series. A v0.2.x server can
roll back to v0.2.(x-1) with the schema at the latest migration —
the older binary works against newer schema. Major bumps
(v0.x → v1.0) may break this contract; that case is called out
explicitly when it lands.

This page is the historical log of upgrade-relevant changes per
release, in reverse-chronological order.

## v0.2.0

**Migrations**: `00027_log_lines_partition`, `00028_log_archive`.

**What changed**:

- `log_lines` is now RANGE-partitioned by month. The table's
  primary key changed from `id BIGSERIAL` to `(job_run_id, seq, at)`
  — the BIGSERIAL is gone. External readers that referenced
  `log_lines.id` need to switch to `(job_run_id, seq)`.
- `log_lines.id` was never exposed via the API, so no caller
  outside the database itself was affected by the PK change.
- Cold archive infrastructure is in place. Disabled by default
  (`GOCDNEXT_LOG_ARCHIVE=auto` enables it iff an artefact
  backend is wired). No data migration; existing runs stay in
  log_lines until archived.

**Manual steps**: none. `helm upgrade` does it all.

**Rollback to v0.1.x**: SAFE. The old binary works against the
partitioned table because the schema additions are pure
extensions. `log_lines.id` doesn't exist anymore — the old
server didn't read or write it directly via the platform code,
so the rollback is silent from the application's perspective.

**Helm value renames**: none. New blocks added (`logs.*`,
`caches.*`, `auth.*`, `extraPlugins`) — `helm upgrade --reuse-values`
still works.

## v0.1.1

**Migrations**: none new.

**What changed**: bug fixes around testcontainers flake (CI
infrastructure) + small UI polish. Nothing user-facing in the
data plane.

**Rollback to v0.1.0**: trivial.

## v0.1.0

Initial public release. No migration history before this — fresh
installs only.

---

## Pattern: how breaking changes are announced

When a release breaks the rollback contract or requires manual
intervention, the release notes lead with one of three banners:

### 🟡 Schema migration with rollback caveat

> "**Migration `00029_x` is forward-compatible** (the new server
> reads + writes both old and new shape) **but rollback to a
> server before this migration requires a data fixup query.** See
> the migration guide before downgrading."

The fixup query lives in the migration guide for that version.

### 🟠 Breaking config change

> "**Helm value `foo.bar` is renamed to `foo.baz` in this release.**
> Old name still works for one minor version + warns at boot;
> remove it before the next bump."

Happens in deprecation cycles, never silent.

### 🔴 Major version bump

> "**v1.0.0** — the platform commits to API stability for v1.x.
> Last chance to land breaking changes is `v0.<final-minor>`;
> after that, only additive changes until v2."

So far: not landed. Don't expect this until 2026 H2 at earliest.

## Working backward — the diff between two versions

```bash
# What migrations between v0.1.0 and v0.2.0?
ls server/migrations/ | sort | sed -n '/00010_/,$p' | head -25

# Which env vars are new in v0.2.0?
git diff v0.1.1..v0.2.0 -- server/internal/config/config.go | grep '+' | grep GOCDNEXT_

# What chart value blocks are new?
git diff v0.1.1..v0.2.0 -- charts/gocdnext/values.yaml | grep '^+' | head
```

The release notes tag for each version captures this curated;
the commands above are the source of truth when the notes are
ambiguous.

## Pre-release tags

Pre-releases (`v0.X.Y-rc1`, `v0.X.Y-beta1`) get the same Helm
chart but the chart appVersion is the pre-release tag verbatim
(`0.X.Y-rc1`). They're not pushed to `latest` on the OCI registry;
operators opt in by version-pinning explicitly.

For your own QA / staging environments running pre-releases:
the rollback semantics still hold — pre-release migrations are
forward-compatible with the prior stable. Don't stack pre-releases
in production; they're for feedback, not deploy targets.

## Long-running test pipelines that span versions

If a pipeline starts on v0.X and the platform is upgraded to
v0.X+1 mid-flight: the run continues. The new server picks up
the run state, dispatches outstanding jobs, finishes. New
features (retention behaviours, archive flags, etc.) take
effect on jobs that haven't dispatched yet — already-running
jobs use the v0.X path through the agent, since the agent's
binary version is what runs them.

If the agent is also upgraded mid-flight, in-flight jobs are
SIGKILL'd (the old agent's pod terminates). They appear as
`failed` with exit code 137. Rerunning works — the platform
re-dispatches against the new agent.

To avoid the SIGKILL window, drain agents before the upgrade:

```bash
kubectl -n gocdnext scale deployment/gocdnext-agent --replicas=0
# wait for in-flight runs to drain (or cancel them)
helm upgrade ... # control plane
kubectl -n gocdnext scale deployment/gocdnext-agent --replicas=2
```
