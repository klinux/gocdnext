---
title: Value Stream Map (VSM)
description: gocdnext's signature feature — visualize the full graph of pipelines and how they chain via upstream materials.
---

The Value Stream Map (VSM) is a per-project visualization of every
pipeline in the project plus the directed edges between them
(via `upstream` materials). It's the GoCD primitive nobody else
implements — and it's the single most useful view for
understanding how a complex CI/CD setup actually flows.

## Why it matters

Modern monorepos chain pipelines: `lint` is independent, `ci-server`
depends on `lint`, `ci-web` depends on `ci-server.test` finishing
successfully, `deploy` depends on both. Reading that out of YAML
files in `.gocdnext/` is doable but tedious; on a 20-pipeline
project, impossible.

The VSM renders the same information as a graph:

- Each pipeline = a node.
- An edge from A → B means "B has an `upstream:` material on A".
- Nodes are colour-coded by the latest run's status.
- Hovering shows the latest run's metrics; clicking jumps to the
  run detail page.

You see at a glance:

- Which pipelines depend on which.
- Where the bottleneck is (slowest stage, dimmest cache hit ratio).
- Where the breakage propagated from (red node lighting up the
  downstream).

## Where it lives

`/projects/<slug>/vsm` in the dashboard. The link is in the
project nav (icon: a graph). One per project; cross-project VSMs
aren't shown today (each project is its own audit boundary).

## How edges are inferred

The graph is built from the `materials` table:

```sql
SELECT pipeline_id, config
FROM materials
WHERE type = 'upstream';
```

`config` JSONB looks like `{"pipeline": "ci-server", "stage":
"test", "status": "success"}`. The graph builder resolves the
referenced pipeline by name within the same project, draws the
edge.

If the referenced pipeline doesn't exist (typo in YAML, was
deleted), the edge is dropped — the VSM only shows live
relationships.

## Status colors

| State | Source | What it means |
|---|---|---|
| Green | latest run = success | Last run was clean |
| Red | latest run = failed | Last run failed; downstream may be holding |
| Yellow | currently running OR awaiting_approval | In flight |
| Grey | never run | Pipeline applied but no runs yet |
| Striped (warning) | bottleneck flag | Stage of this pipeline is the bottleneck of historical runs |

The bottleneck flag aligns with the same heuristic the project
detail page uses (slowest stage in the historical median by p50
duration).

## Layout

The VSM uses a Sugiyama-style layered layout: source pipelines
(no incoming edges) on the left, sinks (no outgoing edges) on
the right, intermediate stages in between. Cycles are detected
and warned about (gocdnext doesn't enforce a DAG at apply time
because the upstream resolution is name-based; an apply that
introduces a cycle shows up as a warning in the VSM).

The graph is drag-pannable + zoomable. State (zoom level, pan
offset) persists per-user via the `user_preferences` table — when
you re-open the page, your last view is restored.

## What you can do from the VSM

- **Click a pipeline node** → jump to that pipeline's detail
  page.
- **Click an edge** → see the trigger metrics for that fanout
  (how often the upstream's stage success → downstream's run
  was created, average gap, last triggered at).
- **Filter by branch**: top-right dropdown lets you scope the
  graph to runs from a specific branch — useful for monorepos
  where main and feature branches have different fanout
  patterns.
- **Status badges live-update** via SSE while a run is in
  flight. You can leave the VSM open during a deploy and watch
  the green wave propagate downstream.

## Limits

- **One project per VSM**. Cross-project chains (a pipeline in
  project A triggering one in project B) aren't supported in
  the YAML — and so don't show up here.
- **Static layout**. The Sugiyama layout is recomputed on every
  load; if you add a pipeline, the layout shifts. There's no
  "save my preferred layout" yet.
- **No metrics overlay**. You see status + bottleneck flag, but
  not detailed run rate / mean duration on the edges. Roadmap.

## Common interpretations

### "All red downstream"

A failure in an upstream pipeline propagates: the next run
downstream doesn't fire (because the upstream's stage didn't
hit `success`). The downstream node stays at its last status
(green from the prior run, usually). When you fix the upstream
and a new run goes through, the green wave propagates.

### "Always yellow"

A pipeline that's always in `running` state — either it's a
long-running pipeline (cron job that takes hours) or it's
stuck. Click in to see the active run; if it's been running
for hours unexpectedly, the agent might've disconnected. The
reaper picks these up automatically after the heartbeat
window — see `internal/scheduler/reaper.go`.

### "Striped warning"

The pipeline's historical median for some stage is way off the
norm. Click in, look at the *bottleneck pill* on the project
detail page — usually one stage is dragging the run duration
disproportionately. Common culprits: cold caches (no `cache:`
block), uncached test fixtures, oversized matrix expansions.

## Common pitfalls

- **Renaming a pipeline breaks edges**: if you rename `ci-server`
  to `server-ci`, every downstream `upstream: { pipeline:
  ci-server }` reference becomes stale. The VSM drops the edge.
  Apply both renames + downstream YAML updates in the same PR.
- **VSM hides projects without pipelines**: if you're on a fresh
  project that hasn't been applied yet, the VSM shows an empty
  state. Apply at least one pipeline to populate.
- **Render time on huge graphs**: 50+ pipelines pushes the
  client-side layout to noticeable latency. There's a virtualised
  rendering plan on the roadmap; for now, big monorepos may
  benefit from splitting into sub-projects.
