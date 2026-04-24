-- name: GetPipelineDefinition :one
-- Returns the pipeline's stored YAML snapshot AND the owning
-- project's notifications list — at run-create time the synth
-- stage needs both (pipeline's own notifications or, when
-- absent, the project-level inherited set). One round-trip is
-- cheaper than two for what's already the hottest path on
-- webhook-heavy workloads.
SELECT pl.id, pl.project_id, pl.name, pl.definition, pl.definition_version, pl.config_path,
       p.notifications AS project_notifications
FROM pipelines pl
JOIN projects p ON p.id = pl.project_id
WHERE pl.id = $1
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
