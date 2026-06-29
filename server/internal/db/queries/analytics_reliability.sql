-- Throughput & reliability rollups for the analytics epic (#107 phase 3),
-- backed by the analytics_run_daily materialized rollup (#128 phase 1).
-- RUN-based (not deploy-based like DORA) — so no environment filter; runs are
-- not environment-scoped. Counts come from the rollup (additive, O(days));
-- the queue-wait/duration medians stay LIVE (a median can't be summed across
-- daily buckets). "Failure" = failed or errored; 'canceled' is excluded.
--
-- Window: "last window_days calendar days". Counts/hotspots use the rollup's
-- DATE column (day > current_date - window_days); the live latency query uses
-- the equivalent sargable timestamp bound on finished_at so both cover the same
-- days and the partial index (00060) still applies.

-- name: ThroughputCounts :many
-- Per label-value group: terminal run counts over the window, from the rollup.
-- A project carrying the key under several values contributes to each (JOIN).
SELECT pl.value AS grp,
       COALESCE(SUM(d.runs_success), 0)::bigint AS runs_success,
       COALESCE(SUM(d.runs_failed), 0)::bigint AS runs_failed
FROM project_labels pl
JOIN pipelines p ON p.project_id = pl.project_id
JOIN analytics_run_daily d ON d.pipeline_id = p.id
WHERE pl.key = sqlc.arg(label_key)
  AND d.day > current_date - sqlc.arg(window_days)::int
GROUP BY pl.value
ORDER BY pl.value;

-- name: ThroughputLatency :many
-- Per label-value group: queue-wait + duration p50, LIVE from runs over the
-- same window. Queue wait = created→start (operator capacity); duration =
-- start→finish (the work). finished_at >= current_date - (window_days-1) is the
-- midnight-aligned, index-friendly equivalent of the rollup's day predicate.
WITH base AS (
    SELECT pl.value AS grp,
           EXTRACT(EPOCH FROM (r.started_at - r.created_at))::double precision AS queue_s,
           EXTRACT(EPOCH FROM (r.finished_at - r.started_at))::double precision AS dur_s
    FROM project_labels pl
    JOIN pipelines p ON p.project_id = pl.project_id
    JOIN runs r ON r.pipeline_id = p.id
    WHERE pl.key = sqlc.arg(label_key)
      AND r.status IN ('success', 'failed', 'errored')
      AND r.finished_at IS NOT NULL
      AND r.finished_at >= current_date - make_interval(days => sqlc.arg(window_days)::int - 1)
)
SELECT grp,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY queue_s)
                FILTER (WHERE queue_s IS NOT NULL), 0)::double precision AS queue_wait_p50_s,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dur_s)
                FILTER (WHERE dur_s IS NOT NULL), 0)::double precision AS duration_p50_s
FROM base
GROUP BY grp
ORDER BY grp;

-- name: ReliabilityHotspots :many
-- The pipelines that break most, among projects carrying label_key, over the
-- window — from the rollup. EXISTS (not a label JOIN) so each pipeline appears
-- once regardless of how many label values its project has. min_runs guards
-- against a 1-of-1 failure topping the list; only pipelines with at least one
-- failure qualify. Ordered worst-first (rate, then absolute failures).
SELECT pr.slug AS project_slug,
       pr.name AS project,
       p.name AS pipeline,
       SUM(d.runs_success + d.runs_failed)::bigint AS runs_total,
       SUM(d.runs_failed)::bigint AS runs_failed,
       (SUM(d.runs_failed)::double precision
            / NULLIF(SUM(d.runs_success + d.runs_failed), 0))::double precision AS failure_rate
FROM analytics_run_daily d
JOIN pipelines p ON p.id = d.pipeline_id
JOIN projects pr ON pr.id = p.project_id
WHERE EXISTS (
        SELECT 1 FROM project_labels pl
        WHERE pl.project_id = p.project_id AND pl.key = sqlc.arg(label_key)
      )
  AND d.day > current_date - sqlc.arg(window_days)::int
GROUP BY pr.slug, pr.name, p.name, p.id
HAVING SUM(d.runs_success + d.runs_failed) >= sqlc.arg(min_runs)::bigint
   AND SUM(d.runs_failed) > 0
ORDER BY failure_rate DESC, runs_failed DESC, runs_total DESC
LIMIT sqlc.arg(max_rows)::int;
