-- Maintenance of the analytics_run_daily rollup (#128 phase 1).

-- name: RefreshRunDaily :exec
-- Recompute + upsert the daily run-outcome buckets for the trailing since_days
-- (whole calendar days, so a partial-day recompute never overwrites a complete
-- bucket with a truncated count). since_days <= 0 recomputes ALL history
-- (the boot backfill). Idempotent: DO UPDATE overwrites with the fresh counts,
-- so re-running or overlapping windows is safe and catches late-finishing runs.
INSERT INTO analytics_run_daily (pipeline_id, day, runs_success, runs_failed)
SELECT r.pipeline_id,
       r.finished_at::date AS day,
       COUNT(*) FILTER (WHERE r.status = 'success')::bigint,
       COUNT(*) FILTER (WHERE r.status IN ('failed', 'errored'))::bigint
FROM runs r
WHERE r.status IN ('success', 'failed', 'errored')
  AND r.finished_at IS NOT NULL
  AND (sqlc.arg(since_days)::int <= 0
       OR r.finished_at >= current_date - make_interval(days => sqlc.arg(since_days)::int))
GROUP BY r.pipeline_id, r.finished_at::date
ON CONFLICT (pipeline_id, day) DO UPDATE
    SET runs_success = EXCLUDED.runs_success,
        runs_failed  = EXCLUDED.runs_failed;
