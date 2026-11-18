-- Build-time percentiles (#107 analytics). "Build run" = a terminal SUCCESS run
-- that produced a ready artifact and did NOT deploy, triggered by something
-- other than a pull request / merge-group probe. This isolates the pipelines
-- that build the artifact+image from PR validation noise (PRs carry lots of
-- variance) and from deploy runs (whose duration is deploy, not build). Duration
-- is start→finish of that run; percentiles are computed LIVE (a median/pXX can't
-- be summed from the daily rollup). The window predicate is the same
-- midnight-aligned, index-friendly form the throughput queries use.

-- name: BuildTimesOverall :one
-- Org-wide build-duration percentiles over the window, across projects that
-- carry the group-by key (so the card matches the rest of the key-scoped page).
WITH base AS (
    SELECT EXTRACT(EPOCH FROM (r.finished_at - r.started_at))::double precision AS dur_s
    FROM runs r
    JOIN pipelines p ON p.id = r.pipeline_id
    WHERE r.status = 'success'
      AND r.started_at IS NOT NULL
      AND r.finished_at IS NOT NULL
      AND r.cause NOT IN ('pull_request', 'merge_group')
      AND r.finished_at >= current_date - make_interval(days => sqlc.arg(window_days)::int - 1)
      AND EXISTS (
            SELECT 1 FROM project_labels pl
            WHERE pl.project_id = p.project_id AND pl.key = sqlc.arg(label_key)
          )
      AND r.produced_artifact
      AND NOT EXISTS (
            SELECT 1 FROM deployment_revisions dr WHERE dr.run_id = r.id
          )
)
SELECT
    COUNT(*)::bigint AS builds,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY dur_s), 0)::double precision AS p50_s,
    COALESCE(PERCENTILE_CONT(0.85) WITHIN GROUP (ORDER BY dur_s), 0)::double precision AS p85_s,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY dur_s), 0)::double precision AS p95_s
FROM base;

-- name: BuildTimesByGroup :many
-- Same build-duration percentiles, split per label-value group (a project under
-- several values of the key contributes to each, via the JOIN).
WITH base AS (
    SELECT pl.value AS grp,
           EXTRACT(EPOCH FROM (r.finished_at - r.started_at))::double precision AS dur_s
    FROM project_labels pl
    JOIN pipelines p ON p.project_id = pl.project_id
    JOIN runs r ON r.pipeline_id = p.id
    WHERE pl.key = sqlc.arg(label_key)
      AND r.status = 'success'
      AND r.started_at IS NOT NULL
      AND r.finished_at IS NOT NULL
      AND r.cause NOT IN ('pull_request', 'merge_group')
      AND r.finished_at >= current_date - make_interval(days => sqlc.arg(window_days)::int - 1)
      AND r.produced_artifact
      AND NOT EXISTS (
            SELECT 1 FROM deployment_revisions dr WHERE dr.run_id = r.id
          )
)
SELECT grp,
       COUNT(*)::bigint AS builds,
       COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY dur_s), 0)::double precision AS p50_s,
       COALESCE(PERCENTILE_CONT(0.85) WITHIN GROUP (ORDER BY dur_s), 0)::double precision AS p85_s,
       COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY dur_s), 0)::double precision AS p95_s
FROM base
GROUP BY grp
ORDER BY grp;
