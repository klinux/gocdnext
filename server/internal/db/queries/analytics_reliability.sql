-- Throughput & reliability rollups for the analytics epic (#107 phase 3).
-- RUN-based (not deploy-based like DORA) — so no environment filter; runs are
-- not environment-scoped. Terminal runs only. "Failure" = failed or errored
-- (infra error); 'canceled' is neither success nor failure and is excluded
-- from the rate denominator. OK to seq-scan runs over the window at gocdnext's
-- scale (internal CI volume); a finished_at index is the lever if it grows.

-- name: ThroughputRollup :many
-- Per label-value group (for key = label_key): run counts + queue-wait and
-- duration medians over the trailing window. A project carrying the key under
-- several values contributes to each (JOIN, like DoraRollup) — intentional, the
-- group is the unit. Queue wait = created→start (operator capacity); duration =
-- start→finish (the work itself).
WITH base AS (
    SELECT pl.value AS grp,
           r.status,
           EXTRACT(EPOCH FROM (r.started_at - r.created_at))::double precision AS queue_s,
           EXTRACT(EPOCH FROM (r.finished_at - r.started_at))::double precision AS dur_s
    FROM project_labels pl
    JOIN pipelines p ON p.project_id = pl.project_id
    JOIN runs r ON r.pipeline_id = p.id
    WHERE pl.key = sqlc.arg(label_key)
      AND r.status IN ('success', 'failed', 'errored')
      AND r.finished_at IS NOT NULL
      AND r.finished_at >= now() - sqlc.arg(since_window)::interval
)
SELECT grp,
       COUNT(*) FILTER (WHERE status = 'success')::bigint AS runs_success,
       COUNT(*) FILTER (WHERE status IN ('failed', 'errored'))::bigint AS runs_failed,
       COUNT(*)::bigint AS runs_total,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY queue_s)
                FILTER (WHERE queue_s IS NOT NULL), 0)::double precision AS queue_wait_p50_s,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dur_s)
                FILTER (WHERE dur_s IS NOT NULL), 0)::double precision AS duration_p50_s
FROM base
GROUP BY grp
ORDER BY grp;

-- name: ReliabilityHotspots :many
-- The pipelines that break most, among projects carrying label_key, over the
-- window. EXISTS (not a label JOIN) so each pipeline appears once regardless of
-- how many label values its project has. min_runs guards against a 1-of-1
-- failure topping the list; only pipelines with at least one failure qualify.
-- Ordered worst-first (rate, then absolute failures).
WITH base AS (
    SELECT p.id AS pipeline_id,
           p.name AS pipeline,
           pr.slug AS project_slug,
           pr.name AS project,
           r.status
    FROM pipelines p
    JOIN projects pr ON pr.id = p.project_id
    JOIN runs r ON r.pipeline_id = p.id
    WHERE EXISTS (
            SELECT 1 FROM project_labels pl
            WHERE pl.project_id = p.project_id AND pl.key = sqlc.arg(label_key)
          )
      AND r.status IN ('success', 'failed', 'errored')
      AND r.finished_at IS NOT NULL
      AND r.finished_at >= now() - sqlc.arg(since_window)::interval
)
SELECT project_slug,
       project,
       pipeline,
       COUNT(*)::bigint AS runs_total,
       COUNT(*) FILTER (WHERE status IN ('failed', 'errored'))::bigint AS runs_failed,
       (COUNT(*) FILTER (WHERE status IN ('failed', 'errored'))::double precision
            / COUNT(*))::double precision AS failure_rate
FROM base
GROUP BY project_slug, project, pipeline
HAVING COUNT(*) >= sqlc.arg(min_runs)::bigint
   AND COUNT(*) FILTER (WHERE status IN ('failed', 'errored')) > 0
ORDER BY failure_rate DESC, runs_failed DESC, runs_total DESC
LIMIT sqlc.arg(max_rows)::int;
