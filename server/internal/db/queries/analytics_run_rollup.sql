-- Maintenance of the analytics_run_daily rollup (#128 phase 1).
--
-- A run's bucket is MUTABLE: RerunJob/RerunRun reopen the same row (finished_at
-- NULL → a new finished_at on completion), so a run can change status and even
-- move to a different day. An additive upsert would leave a stale count in the
-- old bucket (double counting). So a refresh is DELETE-the-window + reinsert
-- (run in one tx by the store) — buckets that lost their last terminal run go to
-- zero. The trailing-window refresh self-corrects recent reruns; a periodic full
-- rebuild (since_days <= 0) heals reruns of runs that finished outside the
-- window. The window predicates here MUST stay aligned (same day range).

-- name: TryRollupLock :one
-- Transaction-scoped advisory lock so only one replica refreshes the rollup at a
-- time (auto-released on commit/rollback). false → another replica holds it;
-- the caller skips this cycle instead of duplicating the scan + write.
SELECT pg_try_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: DeleteRunDailyWindow :exec
-- Clear the buckets the matching InsertRunDailyWindow will rebuild. since_days
-- <= 0 clears ALL history (full rebuild); otherwise the trailing window
-- [current_date - since_days, today].
DELETE FROM analytics_run_daily
WHERE sqlc.arg(since_days)::int <= 0
   OR day >= current_date - sqlc.arg(since_days)::int;

-- name: InsertRunDailyWindow :exec
-- Recompute the daily terminal-run counts for the same window the delete cleared.
-- Bucketed by finished_at::date. runs_failed folds 'failed' + 'errored';
-- 'canceled' is neither and is excluded. ON CONFLICT is a belt-and-suspenders
-- guard (the window was just deleted in this tx and the advisory lock serialises
-- refreshers, so a conflict shouldn't arise).
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
