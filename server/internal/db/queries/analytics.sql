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
           -- Lead time = producing run START → deploy finish (excludes queue
           -- wait, which is operator capacity, not change latency). NULL when
           -- the run never started → filtered out of the median below.
           EXTRACT(EPOCH FROM (dr.finished_at - r.started_at))::double precision AS lead_s
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
-- gap to the next SUCCESS in the same environment. The restore lookup is a
-- lateral index probe per failure instead of a self-scan over all historical
-- deploy events for the label key.
WITH failures AS (
    SELECT pl.value AS grp, dr.environment_id, dr.finished_at
    FROM project_labels pl
    JOIN environments e ON e.project_id = pl.project_id
    JOIN deployment_revisions dr ON dr.environment_id = e.id
    WHERE pl.key = sqlc.arg(label_key)
      AND dr.status = 'failed'
      AND dr.finished_at IS NOT NULL
      AND dr.finished_at >= now() - sqlc.arg(since_window)::interval
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
