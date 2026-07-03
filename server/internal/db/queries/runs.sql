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
-- has_services is a snapshot of `pipeline.Services` non-emptiness
-- (migration 00036). Stamped here so the run-terminal cleanup
-- cascade can decide whether to broadcast CleanupRunServices
-- without re-reading the (possibly-drifted) current pipeline
-- definition. The Go layer (store.insertRunSkeleton) computes the
-- value from the SAME `domain.Pipeline` it just used to materialise
-- stages + jobs — without this, a concurrent ApplyProject between
-- the Go decode and a re-read inside SQL could give us mismatched
-- snapshots under READ COMMITTED.
--
-- service_names (migration 00055) is the same snapshot at name
-- granularity — computed from the SAME def for the same drift-safety
-- reason — so the pipelines list can show WHICH services a run
-- declared, not just whether it declared any. Appended last so the
-- existing positional params keep their order.
-- ref (migration 00065) is the supersede LANE key — the triggering branch,
-- snapshotted at create time from the same trigger context (drift-safe like the
-- others). Appended last so the existing positional params keep their order.
INSERT INTO runs (
    pipeline_id, counter, cause, cause_detail, status, revisions, triggered_by,
    has_services, service_names, ref
) VALUES (
    $1, $2, $3, $4, 'queued', $5, $6, $7, $8, $9
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

-- name: ListJobStatusForRun :many
-- Lean projection (name + matrix_key + status) used by the scheduler's
-- needs-satisfaction gate. Loaded ONCE per dispatch tick and consulted
-- per-candidate to decide "all upstreams green?". A row-per-(name,
-- matrix_key) layout means matrix fanouts surface as multiple rows
-- under the same name, which the gate folds into "all matrix children
-- must succeed" — see scheduler/needs.go for the semantic.
--
-- Ordering matches ListJobRunsByRun so the per-name slices the
-- scheduler builds are deterministic across runs (matrix combos
-- iterate in insert order, which sorted by matrix_key at row creation
-- per store/runs.go).
SELECT name, matrix_key, status
FROM job_runs
WHERE run_id = $1
ORDER BY name, matrix_key NULLS FIRST;

-- name: ListStageRunsByRun :many
SELECT id, run_id, name, ordinal, status
FROM stage_runs
WHERE run_id = $1
ORDER BY ordinal;
