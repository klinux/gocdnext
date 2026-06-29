-- Cross-project analytics rollups, grouped by a project label key (the #107
-- epic). All metrics derive from deployment_revisions + the producing run.

-- name: ListLabelKeys :many
-- Distinct label keys across all projects — the dashboard's "group by" picker.
SELECT DISTINCT key FROM project_labels ORDER BY key;

-- name: ListAnalyticsEnvironments :many
-- Distinct environment names that have terminal deploys and belong to a project
-- carrying the group-by key — the dashboard's "environment" filter options.
SELECT DISTINCT e.name
FROM environments e
WHERE EXISTS (
        SELECT 1 FROM project_labels pl
        WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
    )
  AND EXISTS (
        SELECT 1 FROM deployment_revisions dr
        WHERE dr.environment_id = e.id
          AND dr.status IN ('success', 'failed')
          AND dr.finished_at IS NOT NULL
    )
ORDER BY e.name;

-- name: DoraCountsGroup :many
-- Deploy COUNTS per label-value group over the [since_days, until_days) day
-- window, from the analytics_deploy_daily rollup (additive, O(days)). Pairs with
-- DoraLeadGroup (live lead p50) + DoraMTTR (live). day-window mirrors the live
-- interval window: current = (since=window, until=0); prior = (2×window, window).
SELECT pl.value AS grp,
       COALESCE(SUM(d.deploys_success), 0)::bigint AS deploys_success,
       COALESCE(SUM(d.deploys_total), 0)::bigint AS deploys_total,
       COALESCE(SUM(d.deploys_failed), 0)::bigint AS deploys_failed
FROM project_labels pl
JOIN environments e ON e.project_id = pl.project_id
JOIN analytics_deploy_daily d ON d.environment_id = e.id
WHERE pl.key = sqlc.arg(label_key)
  AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
  AND d.day >  current_date - sqlc.arg(since_days)::int
  AND d.day <= current_date - sqlc.arg(until_days)::int
GROUP BY pl.value
ORDER BY pl.value;

-- name: DoraLeadGroup :many
-- Lead-time p50 per group, LIVE from deploys + producing run (a median can't be
-- summed across rollup buckets). Lead = producing run START → deploy finish
-- (excludes queue wait). Same window as DoraCountsGroup, interval-based so the
-- 00057 index applies.
WITH base AS (
    SELECT pl.value AS grp,
           dr.status,
           EXTRACT(EPOCH FROM (dr.finished_at - r.started_at))::double precision AS lead_s
    FROM project_labels pl
    JOIN environments e ON e.project_id = pl.project_id
    JOIN deployment_revisions dr ON dr.environment_id = e.id
    LEFT JOIN runs r ON r.id = dr.run_id
    WHERE pl.key = sqlc.arg(label_key)
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
      AND dr.status IN ('success', 'failed')
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
      AND dr.finished_at <  now() - sqlc.arg(until_window)::interval
)
SELECT grp,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY lead_s)
                FILTER (WHERE status = 'success' AND lead_s IS NOT NULL),
                0)::double precision AS lead_time_p50_s
FROM base
GROUP BY grp
ORDER BY grp;

-- name: DoraCountsOrg :one
-- Org-wide deploy COUNTS over the [since_days, until_days) day window, from the
-- analytics_deploy_daily rollup. EXISTS (not a join) so an environment is
-- counted once even if its project carries the key under several values.
-- Current window = (since=window, until=0); prior = (2×window, window).
SELECT COALESCE(SUM(d.deploys_success), 0)::bigint AS deploys_success,
       COALESCE(SUM(d.deploys_total), 0)::bigint AS deploys_total,
       COALESCE(SUM(d.deploys_failed), 0)::bigint AS deploys_failed
FROM analytics_deploy_daily d
JOIN environments e ON e.id = d.environment_id
WHERE d.day >  current_date - sqlc.arg(since_days)::int
  AND d.day <= current_date - sqlc.arg(until_days)::int
  AND EXISTS (
      SELECT 1 FROM project_labels pl
      WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
  )
  AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment));

-- name: DoraLeadOrg :one
-- Org-wide lead-time p50, LIVE (percentile can't be summed). Same window as
-- DoraCountsOrg, interval-based for the 00057 index.
WITH base AS (
    SELECT dr.status,
           EXTRACT(EPOCH FROM (dr.finished_at - r.started_at))::double precision AS lead_s
    FROM deployment_revisions dr
    JOIN environments e ON e.id = dr.environment_id
    LEFT JOIN runs r ON r.id = dr.run_id
    WHERE dr.status IN ('success', 'failed')
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
      AND dr.finished_at <  now() - sqlc.arg(until_window)::interval
      AND EXISTS (
          SELECT 1 FROM project_labels pl
          WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
      )
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
)
SELECT COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY lead_s)
                FILTER (WHERE status = 'success' AND lead_s IS NOT NULL),
                0)::double precision AS lead_time_p50_s
FROM base;

-- name: DoraWindowMTTR :one
-- Org-wide median time-to-restore over [since, until): for each FAILED deploy
-- whose project carries the key, the gap to the next SUCCESS in the same
-- environment (lateral index probe per failure).
WITH failures AS (
    SELECT dr.environment_id, dr.finished_at
    FROM deployment_revisions dr
    JOIN environments e ON e.id = dr.environment_id
    WHERE dr.status = 'failed'
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
      AND dr.finished_at <  now() - sqlc.arg(until_window)::interval
      AND EXISTS (
          SELECT 1 FROM project_labels pl
          WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
      )
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
)
SELECT COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
                    ORDER BY EXTRACT(EPOCH FROM (s.finished_at - f.finished_at))::double precision
                ) FILTER (WHERE s.finished_at IS NOT NULL),
                0)::double precision AS mttr_p50_s
FROM failures f
LEFT JOIN LATERAL (
    SELECT drs.finished_at
    FROM deployment_revisions drs
    WHERE drs.environment_id = f.environment_id
      AND drs.status = 'success'
      AND drs.finished_at IS NOT NULL
      AND drs.finished_at > f.finished_at
    ORDER BY drs.finished_at ASC
    LIMIT 1
) s ON TRUE;

-- name: DoraDailyCounts :many
-- Dense per-day org deploy COUNTS over the trailing window, from the rollup —
-- feeds the hero sparklines. generate_series yields one row per calendar day
-- (zero-filled) so a sparse window still plots an honest trend. Pairs with
-- DoraDailyLead (live lead p50 per day), merged by day in the store.
WITH days AS (
    SELECT generate_series(
        current_date - sqlc.arg(since_days)::int,
        current_date,
        interval '1 day'
    )::date AS day
),
agg AS (
    SELECT d.day AS day,
           SUM(d.deploys_success)::bigint AS deploys_success,
           SUM(d.deploys_total)::bigint AS deploys_total,
           SUM(d.deploys_failed)::bigint AS deploys_failed
    FROM analytics_deploy_daily d
    JOIN environments e ON e.id = d.environment_id
    WHERE d.day > current_date - sqlc.arg(since_days)::int
      AND EXISTS (
          SELECT 1 FROM project_labels pl
          WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
      )
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
    GROUP BY d.day
)
SELECT s.day AS day,
       COALESCE(a.deploys_success, 0)::bigint AS deploys_success,
       COALESCE(a.deploys_total, 0)::bigint AS deploys_total,
       COALESCE(a.deploys_failed, 0)::bigint AS deploys_failed
FROM days s
LEFT JOIN agg a ON a.day = s.day
ORDER BY s.day;

-- name: DoraDailyLead :many
-- Per-day org lead-time p50, LIVE (percentile can't be summed). Sparse — only
-- days with successful deploys; the store left-joins these onto the dense
-- DoraDailyCounts day list, defaulting missing days to 0.
SELECT date_trunc('day', dr.finished_at)::date AS day,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
                    ORDER BY EXTRACT(EPOCH FROM (dr.finished_at - r.started_at))::double precision
                ) FILTER (WHERE dr.status = 'success' AND r.started_at IS NOT NULL),
                0)::double precision AS lead_time_p50_s
FROM deployment_revisions dr
JOIN environments e ON e.id = dr.environment_id
LEFT JOIN runs r ON r.id = dr.run_id
WHERE dr.status IN ('success', 'failed')
  AND dr.finished_at IS NOT NULL
  AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
  AND EXISTS (
      SELECT 1 FROM project_labels pl
      WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
  )
  AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
GROUP BY day;

-- name: DoraMTTR :many
-- Median time-to-restore per group: for each FAILED deploy in the window, the
-- gap to the next SUCCESS in the same environment. The restore lookup is a
-- lateral index probe per failure instead of a self-scan over all historical
-- deploy events for the label key.
WITH failures AS (
    SELECT pl.value AS grp, dr.environment_id, dr.finished_at
    FROM project_labels pl
    JOIN environments e ON e.project_id = pl.project_id
    JOIN deployment_revisions dr ON dr.environment_id = e.id
    WHERE pl.key = sqlc.arg(label_key)
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
      AND dr.status = 'failed'
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
      AND dr.finished_at <  now() - sqlc.arg(until_window)::interval
)
SELECT grp,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
                    ORDER BY EXTRACT(EPOCH FROM (s.finished_at - f.finished_at))::double precision
                ) FILTER (WHERE s.finished_at IS NOT NULL),
                0)::double precision AS mttr_p50_s
FROM failures f
LEFT JOIN LATERAL (
    SELECT drs.finished_at
    FROM deployment_revisions drs
    WHERE drs.environment_id = f.environment_id
      AND drs.status = 'success'
      AND drs.finished_at IS NOT NULL
      AND drs.finished_at > f.finished_at
    ORDER BY drs.finished_at ASC
    LIMIT 1
) s ON TRUE
GROUP BY f.grp;

-- name: DoraBottleneck :one
-- Org lead-time decomposition over the trailing window: per-stage p50 across
-- successful, NON-ROLLBACK deploys correlated to a pull request (deployed commit
-- == vcs_pull_requests.merge_sha). Stages are consecutive: Coding (first commit
-- → PR opened), Review (→ approval, only when approved_at exists), Release wait
-- (approval/merge → deploy job start), Deploy (deploy job start → finish).
-- Rollbacks are excluded entirely (a revert isn't new change-delivery and would
-- inflate Release wait against an old approval). `eligible` = every successful
-- non-rollback deploy in the window; `excluded` = those with no PR correlation
-- (incl. retention-pruned runs / no git revision). Each stage exposes its own
-- sample count, since p50s drop rows with missing boundaries.
WITH eligible AS (
    -- Universe = every successful, non-rollback deploy in the window. run/job/
    -- sha/PR are LEFT-joined so a deploy whose run was retention-pruned, or that
    -- has no git revision, or no matching PR, still counts (as `excluded`)
    -- rather than vanishing — deployment_revisions outlives the run on purpose.
    SELECT dr.finished_at AS deploy_finished,
           djr.started_at AS deploy_started,
           pr.first_commit_at, pr.opened_at, pr.approved_at, pr.merged_at,
           (pr.id IS NOT NULL) AS correlated
    FROM deployment_revisions dr
    JOIN environments e ON e.id = dr.environment_id
    LEFT JOIN runs r ON r.id = dr.run_id
    LEFT JOIN job_runs djr ON djr.id = dr.job_run_id
    LEFT JOIN LATERAL (
        -- deployed commit SHA = the git material's revision in runs.revisions
        -- (non-empty branch; skips upstream). Deterministic pick by key so a
        -- multi-material run can't choose a different SHA across calls.
        SELECT rev.v ->> 'revision' AS sha
        FROM jsonb_each(r.revisions) AS rev(k, v)
        WHERE COALESCE(rev.v ->> 'branch', '') <> ''
        ORDER BY rev.k
        LIMIT 1
    ) dep ON TRUE
    LEFT JOIN LATERAL (
        -- One PR per deployed SHA — pick deterministically so mirrored repos
        -- sharing a merge SHA can't fan a deploy into multiple PR rows.
        SELECT vpr.id, vpr.first_commit_at, vpr.opened_at, vpr.approved_at, vpr.merged_at
        FROM vcs_pull_requests vpr
        WHERE vpr.merge_sha <> '' AND vpr.merge_sha = dep.sha
        ORDER BY vpr.merged_at DESC NULLS LAST, vpr.number DESC
        LIMIT 1
    ) pr ON TRUE
    WHERE dr.status = 'success'
      AND NOT dr.is_rollback
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
      AND dr.finished_at <  now()
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
      AND EXISTS (
          SELECT 1 FROM project_labels pl
          WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
      )
)
SELECT
    COUNT(*) FILTER (WHERE correlated)::bigint AS correlated,
    COUNT(*) FILTER (WHERE NOT correlated)::bigint AS excluded,
    COUNT(*) FILTER (WHERE correlated AND first_commit_at IS NOT NULL AND opened_at >= first_commit_at)::bigint AS coding_sample,
    COUNT(*) FILTER (WHERE correlated AND approved_at IS NOT NULL AND opened_at IS NOT NULL AND approved_at >= opened_at)::bigint AS review_sample,
    COUNT(*) FILTER (WHERE correlated AND COALESCE(approved_at, merged_at) IS NOT NULL AND deploy_started >= COALESCE(approved_at, merged_at))::bigint AS release_sample,
    COUNT(*) FILTER (WHERE correlated AND deploy_finished >= deploy_started)::bigint AS deploy_sample,
    -- True end-to-end median (NOT the sum of stage p50s, which come from
    -- different samples). The header shows this; the bar shows the stage split.
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (deploy_finished - first_commit_at))::double precision
    ) FILTER (WHERE correlated AND first_commit_at IS NOT NULL AND deploy_finished >= first_commit_at), 0)::double precision AS total_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (opened_at - first_commit_at))::double precision
    ) FILTER (WHERE correlated AND first_commit_at IS NOT NULL AND opened_at >= first_commit_at), 0)::double precision AS coding_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (approved_at - opened_at))::double precision
    ) FILTER (WHERE correlated AND approved_at IS NOT NULL AND opened_at IS NOT NULL AND approved_at >= opened_at), 0)::double precision AS review_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (deploy_started - COALESCE(approved_at, merged_at)))::double precision
    ) FILTER (WHERE correlated AND COALESCE(approved_at, merged_at) IS NOT NULL AND deploy_started >= COALESCE(approved_at, merged_at)), 0)::double precision AS release_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (deploy_finished - deploy_started))::double precision
    ) FILTER (WHERE correlated AND deploy_finished >= deploy_started), 0)::double precision AS deploy_p50_s
FROM eligible;
