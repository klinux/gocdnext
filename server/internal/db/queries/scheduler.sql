-- name: OtherRunningRunExistsForPipeline :one
-- Returns true when the pipeline has any run in 'running' status
-- other than the one we're about to dispatch. The scheduler checks
-- this for pipelines configured with concurrency: serial and
-- leaves the run queued when another one is already in flight.
-- Excluded self so a re-entrant tick (scheduler evaluates the
-- same run twice) doesn't see itself as a blocker.
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE pipeline_id = $1
      AND status = 'running'
      AND id <> $2
)::boolean AS running;

-- name: ListDispatchableJobs :many
-- Returns queued jobs in the lowest-ordinal stage that still has queued or
-- running work. The scheduler does needs-satisfaction checking in Go so the
-- query stays readable; the stage gate is the only SQL-level constraint.
WITH active_stage AS (
    SELECT MIN(s.ordinal) AS ordinal
    FROM stage_runs s
    WHERE s.run_id = $1 AND s.status IN ('queued', 'running')
)
SELECT j.id, j.run_id, j.stage_run_id, j.name, j.matrix_key, j.image, j.status, j.needs
FROM job_runs j
JOIN stage_runs s ON s.id = j.stage_run_id
WHERE j.run_id = $1
  AND j.status = 'queued'
  AND j.agent_id IS NULL
  AND s.ordinal = (SELECT ordinal FROM active_stage)
ORDER BY j.name, j.matrix_key NULLS FIRST;

-- name: AssignJob :one
-- Moves a queued, unassigned job to running and records the agent. The status
-- predicate prevents a race where two scheduler ticks pick the same job.
UPDATE job_runs
SET status = 'running', agent_id = $2, started_at = NOW()
WHERE id = $1 AND status = 'queued' AND agent_id IS NULL
RETURNING id, run_id, stage_run_id, name, matrix_key, image, status, agent_id;

-- name: MarkRunRunningIfQueued :exec
UPDATE runs
SET status = 'running', started_at = COALESCE(started_at, NOW())
WHERE id = $1 AND status = 'queued';

-- name: MarkStageRunningIfQueued :exec
UPDATE stage_runs
SET status = 'running', started_at = COALESCE(started_at, NOW())
WHERE id = $1 AND status = 'queued';

-- name: ListQueuedRunIDs :many
-- Runs the scheduler's tick reconsiders: both freshly queued ones and
-- already-running runs that may have a queued job waiting (re-queued by the
-- reaper, blocked waiting for a next stage, etc.).
SELECT id FROM runs WHERE status IN ('queued', 'running') ORDER BY created_at;

-- name: GetRunForDispatch :one
-- project_notifications tags along so the dispatcher can resolve
-- synth notification jobs that inherited their spec from the
-- project (pipeline didn't declare `notifications:`). Single
-- round-trip keeps the dispatch hot path tight.
SELECT r.id, r.pipeline_id, p.project_id, r.counter, r.status, r.revisions,
       p.definition, p.config_path,
       pr.notifications AS project_notifications
FROM runs r
JOIN pipelines p ON p.id = r.pipeline_id
JOIN projects pr ON pr.id = p.project_id
WHERE r.id = $1
LIMIT 1;
