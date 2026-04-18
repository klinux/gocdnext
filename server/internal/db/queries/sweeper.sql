-- name: UpdateAgentLastSeen :exec
-- Called from the gRPC heartbeat handler so the reaper can tell which agents
-- are still alive. Tiny write per heartbeat (default cadence 30s).
UPDATE agents SET last_seen_at = NOW() WHERE id = $1;

-- name: ListStaleRunningJobs :many
-- Running jobs whose agent is either offline or hasn't been seen within
-- @staleness. The reaper walks this list every tick and either re-queues or
-- fails them.
SELECT j.id, j.run_id, j.stage_run_id, j.name, j.attempt, j.agent_id,
       a.status AS agent_status, a.last_seen_at
FROM job_runs j
JOIN agents a ON a.id = j.agent_id
WHERE j.status = 'running'
  AND (a.status = 'offline' OR a.last_seen_at IS NULL
       OR a.last_seen_at < NOW() - @staleness::INTERVAL);

-- name: ReclaimJobForRetry :one
-- Flips a running job back to queued, IF it is still running AND still under
-- the retry cap. Returns the new attempt number; ErrNoRows signals the caller
-- should take a different code path (failed-at-max or already-handled).
UPDATE job_runs
SET status = 'queued', agent_id = NULL, started_at = NULL, finished_at = NULL,
    exit_code = NULL, error = NULL, attempt = attempt + 1
WHERE id = $1 AND status = 'running' AND attempt < @max_attempts::int
RETURNING id, run_id, stage_run_id, name, attempt;

-- name: DeleteLogLinesByJob :exec
-- Called after a successful reclaim so the retry starts with a clean log
-- window. Loses the old attempt's output — acceptable MVP trade for not
-- growing the schema to carry per-attempt log namespacing.
DELETE FROM log_lines WHERE job_run_id = $1;

-- name: GetJobAttempt :one
-- Used by the reaper to disambiguate "at max attempts" from "already
-- handled" when ReclaimJobForRetry returned no rows.
SELECT id, status, attempt FROM job_runs WHERE id = $1 LIMIT 1;
