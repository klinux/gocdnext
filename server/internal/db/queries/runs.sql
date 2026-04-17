-- name: GetPipelineDefinition :one
SELECT id, project_id, name, definition, definition_version, config_path
FROM pipelines
WHERE id = $1
LIMIT 1;

-- name: NextRunCounter :one
SELECT (COALESCE(MAX(counter), 0) + 1)::BIGINT AS next
FROM runs
WHERE pipeline_id = $1;

-- name: InsertRun :one
INSERT INTO runs (
    pipeline_id, counter, cause, cause_detail, status, revisions, triggered_by
) VALUES (
    $1, $2, $3, $4, 'queued', $5, $6
)
RETURNING id, pipeline_id, counter, cause, status, created_at;

-- name: InsertStageRun :one
INSERT INTO stage_runs (run_id, name, ordinal)
VALUES ($1, $2, $3)
RETURNING id, run_id, name, ordinal, status;

-- name: InsertJobRun :one
INSERT INTO job_runs (run_id, stage_run_id, name, matrix_key, image, needs)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, run_id, stage_run_id, name, matrix_key, image, status, needs;

-- name: CountRunsByPipeline :one
SELECT COUNT(*) FROM runs WHERE pipeline_id = $1;

-- name: ListJobRunsByRun :many
SELECT id, run_id, stage_run_id, name, matrix_key, image, status, needs
FROM job_runs
WHERE run_id = $1
ORDER BY name, matrix_key NULLS FIRST;

-- name: ListStageRunsByRun :many
SELECT id, run_id, name, ordinal, status
FROM stage_runs
WHERE run_id = $1
ORDER BY ordinal;
