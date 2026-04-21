-- name: ListProjectsWithCounts :many
-- Project list used by the dashboard home. Joins enough aggregate info so the
-- UI renders without round-tripping per row.
--
-- Extra columns feed the card-style layout on /projects:
--   * provider: from the bound scm_source when present; empty string
--     when the project has no repo yet (shown as "—" in the UI).
--   * run_count: total runs across all pipelines — the card shows
--     "5 pipelines · 123 runs" as a density signal.
--   * status_agg: coarse health derived from the most recent run per
--     pipeline. Pre-computed in a CTE (latest_per_pipeline) so the
--     outer aggregate stays aggregate-only; correlated subqueries
--     inside CASE/WHEN during GROUP BY are fragile in Postgres and
--     blew up the previous version of this query.
WITH
latest_per_pipeline AS (
  SELECT DISTINCT ON (r.pipeline_id)
         r.pipeline_id, pl.project_id, r.status AS latest_status
  FROM runs r
  JOIN pipelines pl ON pl.id = r.pipeline_id
  ORDER BY r.pipeline_id, r.created_at DESC
),
project_health AS (
  SELECT project_id,
         bool_or(latest_status = 'running' OR latest_status = 'queued') AS any_running,
         bool_or(latest_status = 'failed') AS any_failed
  FROM latest_per_pipeline
  GROUP BY project_id
)
SELECT p.id, p.slug, p.name, p.description, p.config_path,
       p.created_at, p.updated_at,
       COALESCE(s.provider, '')::TEXT AS provider,
       COUNT(DISTINCT pl.id)::BIGINT AS pipeline_count,
       COUNT(r.id)::BIGINT AS run_count,
       MAX(r.created_at)::TIMESTAMPTZ AS latest_run_at,
       (
         CASE
           WHEN COUNT(DISTINCT pl.id) = 0 THEN 'no_pipelines'
           WHEN MAX(r.created_at) IS NULL THEN 'never_run'
           WHEN COALESCE(ph.any_running, false) THEN 'running'
           WHEN COALESCE(ph.any_failed, false) THEN 'failing'
           ELSE 'success'
         END
       )::TEXT AS status_agg
FROM projects p
LEFT JOIN pipelines pl ON pl.project_id = p.id
LEFT JOIN runs r ON r.pipeline_id = pl.id
LEFT JOIN scm_sources s ON s.project_id = p.id
LEFT JOIN project_health ph ON ph.project_id = p.id
GROUP BY p.id, s.provider, ph.any_running, ph.any_failed
ORDER BY p.updated_at DESC;

-- name: ListTopPipelinesPerProject :many
-- Per-project top-N (most recently updated) pipelines with their
-- latest run status. Used to render the "mini stack" inside each
-- project card without issuing one query per card (N+1). Currently
-- capped at 3 entries per project via ROW_NUMBER partition — the
-- UI appends a "+K more" link when pipeline_count > 3.
--
-- Also returns:
--   * latest_run_id: zero-UUID when the pipeline has never run.
--     Callers collect these to batch-load stage_runs.
--   * definition: the pipeline's YAML snapshot (JSONB). The card
--     renders grey "pending" stage pills from definition.Stages
--     when latest_run_id is zero, and reconciles definition vs.
--     stage_runs for mid-run/cancelled states.
SELECT pipeline_id, project_id, name, latest_run_id,
       latest_run_status, latest_run_at, definition
FROM (
  SELECT pl.id AS pipeline_id, pl.project_id, pl.name,
         pl.definition,
         -- COALESCE to empty string so sqlc types this as
         -- plain string (not *string). Empty means "never run" —
         -- the store layer treats "" the same way null would.
         COALESCE(lr.status, '')::TEXT AS latest_run_status,
         lr.created_at AS latest_run_at,
         -- Nil UUID when the LEFT JOIN LATERAL didn't match a run.
         -- Store-side treats that as "skip stage_runs lookup".
         COALESCE(lr.id, '00000000-0000-0000-0000-000000000000'::uuid) AS latest_run_id,
         ROW_NUMBER() OVER (
           PARTITION BY pl.project_id
           ORDER BY
             -- Failing pipelines first so the card headlines the
             -- problem state; then running, then anything else
             -- that has at least one run, ordered by recency;
             -- finally pipelines that never ran, alphabetical.
             -- Without this priority ladder, a project with 3
             -- freshly-applied-never-ran pipelines and 1 failing
             -- one would hide the failure behind "+N more".
             CASE lr.status
               WHEN 'failed' THEN 0
               WHEN 'running' THEN 1
               WHEN 'queued' THEN 2
               WHEN 'canceled' THEN 3
               WHEN 'success' THEN 4
               ELSE 5 -- never run
             END,
             lr.created_at DESC NULLS LAST,
             pl.name
         ) AS rn
  FROM pipelines pl
  LEFT JOIN LATERAL (
    SELECT id, status, created_at
    FROM runs
    WHERE pipeline_id = pl.id
    ORDER BY created_at DESC
    LIMIT 1
  ) lr ON true
) ranked
WHERE rn <= 3
ORDER BY project_id, rn;

-- name: GetProjectBySlug :one
SELECT id, slug, name, description, config_path, created_at, updated_at
FROM projects
WHERE slug = $1
LIMIT 1;

-- name: ListPipelinesByProjectSlug :many
-- Returns definition alongside metadata so the card can pull
-- stage names from the YAML when the pipeline has never run
-- (no stage_runs yet → no history to render from).
SELECT pl.id, pl.name, pl.definition_version, pl.definition, pl.updated_at
FROM pipelines pl
JOIN projects p ON p.id = pl.project_id
WHERE p.slug = $1
ORDER BY pl.name;

-- name: ListRunsByProjectSlug :many
SELECT r.id, r.pipeline_id, pl.name AS pipeline_name,
       r.counter, r.cause, r.status,
       r.created_at, r.started_at, r.finished_at, r.triggered_by
FROM runs r
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE p.slug = $1
ORDER BY r.created_at DESC
LIMIT $2;

-- name: ListMaterialsByProjectSlug :many
-- All materials across pipelines of a project. VSM uses the
-- `upstream` ones to build edges between pipeline nodes; git ones
-- are informational (shown as entry points on the graph).
SELECT m.pipeline_id, m.type, m.config, m.fingerprint
FROM materials m
JOIN pipelines pl ON pl.id = m.pipeline_id
JOIN projects p  ON p.id  = pl.project_id
WHERE p.slug = $1
ORDER BY m.pipeline_id, m.type;

-- name: ListJobRunsForRuns :many
-- Batch-loads job_runs for every run whose id is in the input
-- array. The project detail page renders a GitLab-style pipeline
-- flow per pipeline, each stage box listing its jobs — fetching
-- these per pipeline would mean N queries. This single scan
-- covers the card set.
SELECT run_id, stage_run_id, id, name, status, started_at, finished_at
FROM job_runs
WHERE run_id = ANY($1::uuid[])
ORDER BY run_id, stage_run_id, name;

-- name: ListStageRunsForRuns :many
-- Batch-loads stage_runs for every run whose id is in the input
-- array — the project detail page renders one pipeline card per
-- pipeline and each card needs the latest run's stage states.
-- Issuing one query per card would mean N+1 round trips; this
-- amortizes them into a single scan.
SELECT run_id, id, name, ordinal, status, started_at, finished_at
FROM stage_runs
WHERE run_id = ANY($1::uuid[])
ORDER BY run_id, ordinal;

-- name: LatestRunPerPipelineByProjectSlug :many
-- DISTINCT ON picks the most recent run per pipeline. Pipelines with
-- no runs yet are absent from the result; the handler merges with
-- ListPipelinesByProjectSlug to produce node entries.
SELECT DISTINCT ON (r.pipeline_id)
  r.pipeline_id, r.id, r.counter, r.cause, r.status,
  r.created_at, r.started_at, r.finished_at, r.triggered_by
FROM runs r
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p  ON p.id  = pl.project_id
WHERE p.slug = $1
ORDER BY r.pipeline_id, r.created_at DESC;

-- name: GetRunWithPipeline :one
SELECT r.id, r.pipeline_id, pl.name AS pipeline_name, p.slug AS project_slug,
       r.counter, r.cause, r.cause_detail, r.status, r.revisions,
       r.created_at, r.started_at, r.finished_at, r.triggered_by
FROM runs r
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE r.id = $1
LIMIT 1;

-- name: ListStageRunsByRunOrdered :many
SELECT id, name, ordinal, status, started_at, finished_at
FROM stage_runs
WHERE run_id = $1
ORDER BY ordinal;

-- name: ListJobRunsByRunFull :many
SELECT id, stage_run_id, name, matrix_key, image,
       status, exit_code, error, started_at, finished_at, agent_id
FROM job_runs
WHERE run_id = $1
ORDER BY name, matrix_key NULLS FIRST;

-- name: TailLogLinesByJob :many
-- Returns the tail (up to $2 lines) of a job's logs, oldest-first within the
-- returned window, so the UI can append-only render.
SELECT seq, stream, at, text
FROM (
    SELECT id, seq, stream, at, text
    FROM log_lines
    WHERE job_run_id = $1
    ORDER BY seq DESC
    LIMIT $2
) recent
ORDER BY seq;
