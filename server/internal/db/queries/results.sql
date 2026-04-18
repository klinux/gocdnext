-- name: InsertLogLine :exec
-- Agents send log lines with a per-(job_run_id) monotonic seq; the UNIQUE
-- constraint makes retries safe.
INSERT INTO log_lines (job_run_id, seq, stream, at, text)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (job_run_id, seq) DO NOTHING;

-- name: CompleteJobRun :one
-- Flips a queued or running job to its terminal state. Idempotent: matches
-- only non-terminal rows. Accepting 'queued' lets the scheduler fail a job
-- at dispatch time (e.g. unresolved secret) without first flipping it to
-- running via AssignJob. Returns stage/run ids so the caller can cascade.
UPDATE job_runs
SET status = $2, exit_code = $3, error = $4, finished_at = NOW()
WHERE id = $1 AND status IN ('queued', 'running')
RETURNING id, run_id, stage_run_id, agent_id, name;

-- name: GetStageProgress :one
-- Counts jobs still working vs already-failed within a stage — the numbers
-- the caller uses to decide whether to promote the stage.
SELECT
    COUNT(*) FILTER (WHERE status IN ('queued', 'running'))::BIGINT AS unfinished,
    COUNT(*) FILTER (WHERE status = 'failed')::BIGINT               AS failed
FROM job_runs
WHERE stage_run_id = $1;

-- name: CompleteStageRun :exec
UPDATE stage_runs
SET status = $2, finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status IN ('queued', 'running');

-- name: GetRunProgress :one
SELECT
    COUNT(*) FILTER (WHERE status IN ('queued', 'running'))::BIGINT AS unfinished,
    COUNT(*) FILTER (WHERE status = 'failed')::BIGINT               AS failed
FROM stage_runs
WHERE run_id = $1;

-- name: CompleteRun :exec
UPDATE runs
SET status = $2, finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status IN ('queued', 'running');

-- name: CancelQueuedStagesInRun :exec
-- When a stage fails we stop dispatching the rest of the run. Running work
-- stays untouched; the agent will still report its outcome.
UPDATE stage_runs
SET status = 'canceled', finished_at = COALESCE(finished_at, NOW())
WHERE run_id = $1 AND status = 'queued';

-- name: CancelQueuedJobsInRun :exec
UPDATE job_runs
SET status = 'canceled', finished_at = COALESCE(finished_at, NOW())
WHERE run_id = $1 AND status = 'queued';
