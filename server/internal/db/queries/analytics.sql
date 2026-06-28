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

-- name: DoraRollup :many
-- DORA metrics per label-value group (for label key = label_key) over the
-- trailing window. Joins each project's labels → environments →
-- deployment_revisions (+ the producing run for lead time). One row per
-- distinct value of the key.
WITH base AS (
    SELECT pl.value AS grp,
           dr.status,
           dr.is_rollback,
           -- Lead time = producing run START → deploy finish (excludes queue
           -- wait, which is operator capacity, not change latency). NULL when
           -- the run never started → filtered out of the median below.
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
       COUNT(*) FILTER (WHERE status = 'success')::bigint AS deploys_success,
       COUNT(*)::bigint AS deploys_total,
       COUNT(*) FILTER (WHERE status = 'failed' OR is_rollback)::bigint AS deploys_failed,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY lead_s)
                FILTER (WHERE status = 'success' AND lead_s IS NOT NULL),
                0)::double precision AS lead_time_p50_s
FROM base
GROUP BY grp
ORDER BY grp;

-- name: DoraWindowAgg :one
-- Org-wide DORA counts + lead-time p50 over an arbitrary [since, until) window
-- (both intervals trailing from now). The same query serves the current window
-- (until=0) and the prior window (since=2×window, until=window) for deltas.
-- A deploy counts once even if its project carries the key under several values
-- (EXISTS, not a join, so no fan-out double count).
WITH base AS (
    SELECT dr.status,
           dr.is_rollback,
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
SELECT COUNT(*) FILTER (WHERE status = 'success')::bigint AS deploys_success,
       COUNT(*)::bigint AS deploys_total,
       COUNT(*) FILTER (WHERE status = 'failed' OR is_rollback)::bigint AS deploys_failed,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY lead_s)
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

-- name: DoraDailySeries :many
-- Dense per-day org buckets over the trailing window — feeds the hero
-- sparklines. generate_series yields one row per calendar day (zero-filled for
-- days with no deploy) so a sparse 90-day window still plots an honest,
-- non-compressed trend. Success/total/failed counts + lead-time p50 per day.
WITH days AS (
    SELECT generate_series(
        date_trunc('day', now() - sqlc.arg(since_window)::interval),
        date_trunc('day', now()),
        interval '1 day'
    )::date AS day
),
agg AS (
    SELECT date_trunc('day', dr.finished_at)::date AS day,
           COUNT(*) FILTER (WHERE dr.status = 'success')::bigint AS deploys_success,
           COUNT(*)::bigint AS deploys_total,
           COUNT(*) FILTER (WHERE dr.status = 'failed' OR dr.is_rollback)::bigint AS deploys_failed,
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
    GROUP BY day
)
SELECT d.day AS day,
       COALESCE(a.deploys_success, 0)::bigint AS deploys_success,
       COALESCE(a.deploys_total, 0)::bigint AS deploys_total,
       COALESCE(a.deploys_failed, 0)::bigint AS deploys_failed,
       COALESCE(a.lead_time_p50_s, 0)::double precision AS lead_time_p50_s
FROM days d
LEFT JOIN agg a ON a.day = d.day
ORDER BY d.day;

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
-- successful deploys correlated to a pull request (the deployed commit ==
-- vcs_pull_requests.merge_sha). Stages are consecutive: Coding (first commit →
-- PR opened), Review (→ approval, only when approved_at exists), Release wait
-- (approval/merge → deploy job start), Deploy (deploy job start → finish).
-- `correlated` is the sample; the caller derives "excluded = success − correlated".
WITH deploys AS (
    SELECT dr.finished_at AS deploy_finished,
           djr.started_at AS deploy_started,
           vpr.first_commit_at, vpr.opened_at, vpr.approved_at, vpr.merged_at
    FROM deployment_revisions dr
    JOIN runs r ON r.id = dr.run_id
    JOIN environments e ON e.id = dr.environment_id
    JOIN job_runs djr ON djr.id = dr.job_run_id
    JOIN LATERAL (
        -- deployed commit SHA = the git material's revision in runs.revisions
        -- (the entry with a non-empty branch; skips upstream materials).
        SELECT rev.value ->> 'revision' AS sha
        FROM jsonb_each(r.revisions) AS rev
        WHERE COALESCE(rev.value ->> 'branch', '') <> ''
        LIMIT 1
    ) dep ON TRUE
    JOIN vcs_pull_requests vpr ON vpr.merge_sha <> '' AND vpr.merge_sha = dep.sha
    WHERE dr.status = 'success'
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
      AND djr.started_at IS NOT NULL
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment))
      AND EXISTS (
          SELECT 1 FROM project_labels pl
          WHERE pl.project_id = e.project_id AND pl.key = sqlc.arg(label_key)
      )
)
SELECT
    COUNT(*)::bigint AS correlated,
    COUNT(*) FILTER (WHERE approved_at IS NOT NULL)::bigint AS review_sample,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (opened_at - first_commit_at))::double precision
    ) FILTER (WHERE first_commit_at IS NOT NULL AND opened_at >= first_commit_at), 0)::double precision AS coding_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (approved_at - opened_at))::double precision
    ) FILTER (WHERE approved_at IS NOT NULL AND opened_at IS NOT NULL AND approved_at >= opened_at), 0)::double precision AS review_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (deploy_started - COALESCE(approved_at, merged_at)))::double precision
    ) FILTER (WHERE COALESCE(approved_at, merged_at) IS NOT NULL AND deploy_started >= COALESCE(approved_at, merged_at)), 0)::double precision AS release_p50_s,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (deploy_finished - deploy_started))::double precision
    ) FILTER (WHERE deploy_finished >= deploy_started), 0)::double precision AS deploy_p50_s
FROM deploys;
