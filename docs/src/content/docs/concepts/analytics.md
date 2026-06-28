---
title: DORA analytics
description: "Org-level DORA metrics — deployment frequency, lead time, change failure rate, time to restore — rolled up across projects, grouped by a project label, classified into performance tiers."
---

The **Analytics** page turns your deploy history into the four
[DORA](https://dora.dev/) metrics, consolidated across every project and
grouped by a label you choose (team, tier, domain, …). It answers a manager's
questions before the raw numbers: *which groups ship fast, which break, and
where is delivery stalling?*

All metrics derive from **deploy markers** — there is nothing extra to
instrument. If a job carries a [`deploy:` block](/concepts/deployments/), every
run that reaches it feeds Analytics.

## Prerequisites

1. **Deploy markers.** At least one pipeline must run a job with a `deploy:`
   block, so gocdnext records `version` → `environment` outcomes. See
   [Deployments & rollback](/concepts/deployments/).
2. **Project labels.** Metrics are grouped by a free-form `key:value` label on
   each project (e.g. `team:payments`, `tier:critical`). Add labels in
   **Project → Settings**. A project with no labels still deploys, but it won't
   appear under any group.

Open the dashboard from the sidebar (**Analytics**). The toolbar drives
everything: **Group by** (label key), **Window** (7 / 30 / 90 days), and
**Environment**.

## The four metrics

Every metric is computed over the selected trailing window and, where a median
is involved, as the **p50** so one outlier deploy can't skew the picture.

### Deployment frequency

How often a group ships **successfully**.

```
deploy_frequency = successful deploys ÷ window days
```

Only `success` deploys count — a failed attempt is not a delivery. Shown as
`/day` on the hero card and `/week` in the leaderboard.

### Lead time for changes

How long a change takes to reach production, measured from the **producing run
starting** to the **deploy finishing**:

```
lead_time = p50( deploy.finished_at − run.started_at )   (successful deploys)
```

We start the clock at `run.started_at`, not `created_at`, so **queue wait**
(time spent waiting for an agent — operator capacity, not change latency) is
excluded. Decomposing the *pre-merge* portion (coding, review) needs VCS
timing gocdnext does not yet persist; that breakdown is planned separately.

### Change failure rate (CFR)

The share of deploys that represented a failure in production:

```
change_failure_rate = (status = 'failed' OR is_rollback) ÷ total deploys
```

A **rollback counts as a change failure** — a deploy you had to revert means a
prior change broke production, even though the rollback itself "succeeded". This
is why the deploy-frequency chart paints rollbacks red.

### Time to restore (MTTR)

How quickly a group recovers after a failed deploy:

```
mttr = p50( next successful deploy.finished_at − failed deploy.finished_at )
```

For each failed deploy, we find the **next success in the same environment** and
take the gap; the metric is the median across those gaps. A group with no
failures in the window shows `—` (no sample), not a misleading "Elite".

## Performance tiers

Each metric is classified against the standard DORA benchmark:

| Metric | Elite | High | Medium | Low |
|---|---|---|---|---|
| Deploy frequency | on-demand | 1×day–1×wk | 1×wk–1×mo | < 1×mo |
| Lead time | < 1 day | 1 day–1 wk | 1 wk–1 mo | > 1 mo |
| Change failure rate | 0–15% | 16–30% | 31–45% | > 45% |
| Time to restore | < 1 hour | < 1 day | < 1 wk | > 1 wk |

A group's **overall tier is its weakest metric** — a team that ships many times
a day but reverts 60% of deploys lands **Low**, not High, because instability is
the constraint a manager needs to see. The same rule gives the org its
"performer" band at the top of the page. Metrics without a sample (no successful
deploy, no restore) are excluded from the verdict rather than counted as Elite.

## Window & comparison

The window (7 / 30 / 90 days) sets the aggregation period. Every value is
compared against the **immediately preceding window of equal length** — a 30-day
view compares the last 30 days to the 30 before that — and the delta is shown as
a coloured pill. Polarity is by *meaning*, not sign: a falling lead time, CFR,
or MTTR is **good** (green); a rising CFR is **bad** (red).

## Environment filter

By default Analytics aggregates **all** deploy environments. Pick one from the
**Environment** selector to scope the entire view — rollup, deltas, daily
series, and leaderboard — to that environment (e.g. only `prod`). The list shows
the environments that actually have deploys under the active group-by key.

## What's on the page

- **Organization summary** — four hero cards (one per metric) with the value,
  performance tier, delta vs. the prior window, a trend sparkline, and the Elite
  threshold for reference. The section header carries the org's overall band.
- **Trend → Deploy frequency** — deploys per day as stacked bars (successful
  teal, change failures red on top), with the window average and an *N change
  failures in M deploys* summary.
- **Performance by `<key>`** — a sortable leaderboard ranking every group across
  all four metrics plus its tier. Click a column header to sort; click again to
  flip direction.
- **Highlights** — the window's biggest improvement, biggest regression, and a
  watch item (stalled cadence), derived per-group from the current vs. prior
  window. Captions are data-derived, not editorial.
- **DORA benchmark reference** — the threshold table above, always on the page.

## How to use it

1. **Label your projects** by the dimension you want to compare (`team:…`,
   `tier:…`, `domain:…`).
2. **Run real deploys** through a `deploy:` job so markers accumulate.
3. Open **Analytics**, pick the **Group by** key and a **Window**, optionally
   narrow to one **Environment**.
4. Read the **org band** and hero deltas for the headline, scan the
   **leaderboard** for the weakest groups, and act on **Highlights** —
   especially regressions and watch items.

## "No data" / empty states

- *No project labels yet* — add a `key:value` label in **Project → Settings**.
- *No deploys in this window* — the group's projects haven't run a `deploy:`
  job in the period; widen the window or check the pipeline.
- A metric showing `—` means **no sample** (e.g. MTTR with zero failures), which
  is reported honestly rather than as a perfect score.

## API

The dashboard reads a single endpoint; the same data is available to scripts:

- `GET /api/v1/analytics/dora/overview?key=&window_days=&environment=` — org
  rollup (current + prior window), daily series, and per-group leaderboard.
- `GET /api/v1/analytics/dora?key=&window_days=&environment=` — per-group rollup
  only.
- `GET /api/v1/analytics/label-keys` and `GET /api/v1/analytics/environments?key=`
  — the group-by and environment options.

See the [API reference](/reference/api/) for full schemas.
