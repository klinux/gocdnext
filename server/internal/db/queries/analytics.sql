-- Cross-project analytics rollups, grouped by a project label key (the #107
-- epic). All metrics derive from deployment_revisions + the producing run.

-- name: ListLabelKeys :many
-- Distinct label keys across all projects — the dashboard's "group by" picker.
SELECT DISTINCT key FROM project_labels ORDER BY key;

-- name: DoraRollup :many
-- DORA metrics per label-value group (for label key = label_key) over the
-- trailing window. Joins each project's labels → environments →
-- deployment_revisions (+ the producing run for lead time). One row per
-- distinct value of the key.
WITH base AS (
    SELECT pl.value AS grp,
           dr.status,
           dr.is_rollback,
           EXTRACT(EPOCH FROM (dr.finished_at - r.created_at))::double precision AS lead_s
    FROM project_labels pl
    JOIN environments e ON e.project_id = pl.project_id
    JOIN deployment_revisions dr ON dr.environment_id = e.id
    LEFT JOIN runs r ON r.id = dr.run_id
    WHERE pl.key = sqlc.arg(label_key)
      AND dr.status IN ('success', 'failed')
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
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

-- name: DoraMTTR :many
-- Median time-to-restore per group: for each FAILED deploy in the window, the
-- gap to the next SUCCESS in the same environment. Successes are searched with
-- no upper bound so a restore just after the window still counts.
WITH ev AS (
    SELECT pl.value AS grp, dr.environment_id, dr.status, dr.finished_at
    FROM project_labels pl
    JOIN environments e ON e.project_id = pl.project_id
    JOIN deployment_revisions dr ON dr.environment_id = e.id
    WHERE pl.key = sqlc.arg(label_key)
      AND dr.status IN ('success', 'failed')
      AND dr.finished_at IS NOT NULL
),
restores AS (
    SELECT f.grp,
           EXTRACT(EPOCH FROM (
             (SELECT MIN(s.finished_at) FROM ev s
               WHERE s.environment_id = f.environment_id
                 AND s.status = 'success'
                 AND s.finished_at > f.finished_at) - f.finished_at
           ))::double precision AS restore_s
    FROM ev f
    WHERE f.status = 'failed'
      AND f.finished_at >= now() - sqlc.arg(since_window)::interval
)
SELECT grp,
       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY restore_s)
                FILTER (WHERE restore_s IS NOT NULL),
                0)::double precision AS mttr_p50_s
FROM restores
GROUP BY grp;
