-- Maintenance of the analytics_deploy_daily rollup (#128 phase 1b) — the
-- deploy/DORA mirror of analytics_run_rollup. Same DELETE-window + reinsert
-- contract (deploys are mutable via rollback/redeploy), same leader advisory
-- lock (TryRollupLock, shared with the run rollup). since_days <= 0 = full
-- rebuild.

-- name: DeleteDeployDailyWindow :exec
DELETE FROM analytics_deploy_daily
WHERE sqlc.arg(since_days)::int <= 0
   OR day >= current_date - sqlc.arg(since_days)::int;

-- name: InsertDeployDailyWindow :exec
-- deploys_failed folds status='failed' OR is_rollback (a rollback signals a
-- change failure even when the rollback deploy itself succeeded) — matches the
-- live DORA queries. deploys_total counts terminal deploys (success+failed).
INSERT INTO analytics_deploy_daily (environment_id, day, deploys_success, deploys_total, deploys_failed)
SELECT dr.environment_id,
       dr.finished_at::date AS day,
       COUNT(*) FILTER (WHERE dr.status = 'success')::bigint,
       COUNT(*)::bigint,
       COUNT(*) FILTER (WHERE dr.status = 'failed' OR dr.is_rollback)::bigint
FROM deployment_revisions dr
WHERE dr.status IN ('success', 'failed')
  AND dr.finished_at IS NOT NULL
  AND (sqlc.arg(since_days)::int <= 0
       OR dr.finished_at >= current_date - make_interval(days => sqlc.arg(since_days)::int))
GROUP BY dr.environment_id, dr.finished_at::date
ON CONFLICT (environment_id, day) DO UPDATE
    SET deploys_success = EXCLUDED.deploys_success,
        deploys_total   = EXCLUDED.deploys_total,
        deploys_failed  = EXCLUDED.deploys_failed;
