-- name: ListProjectsWithCounts :many
-- Project list used by the dashboard home. Joins enough aggregate info so the
-- UI renders without round-tripping per row.
SELECT p.id, p.slug, p.name, p.description, p.config_path,
       p.created_at, p.updated_at,
       COUNT(DISTINCT pl.id)::BIGINT AS pipeline_count,
       MAX(r.created_at)::TIMESTAMPTZ AS latest_run_at
FROM projects p
LEFT JOIN pipelines pl ON pl.project_id = p.id
LEFT JOIN runs r ON r.pipeline_id = pl.id
GROUP BY p.id
ORDER BY p.updated_at DESC;

-- name: GetProjectBySlug :one
SELECT id, slug, name, description, config_path, created_at, updated_at
FROM projects
WHERE slug = $1
LIMIT 1;

-- name: ListPipelinesByProjectSlug :many
SELECT pl.id, pl.name, pl.definition_version, pl.updated_at
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
