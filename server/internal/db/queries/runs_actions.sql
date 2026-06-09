-- name: GetRunForAction :one
-- Thin row used by cancel/rerun handlers to check status + find the
-- pipeline + revisions without pulling the whole detail query.
SELECT id, pipeline_id, status, revisions
FROM runs
WHERE id = $1;

-- name: CancelActiveRun :one
-- Flips a run to 'canceled' only if it was still active. Idempotent:
-- a second call on a terminal run returns no rows so the handler
-- can answer 409. Returns the row id so the caller can tell the
-- update happened.
--
-- queue_reason is cleared in the same UPDATE so a canceled-while-
-- queued run doesn't carry a "waiting on #N" message into the
-- runs list. Doing it in this UPDATE (vs a follow-up
-- ClearRunQueueReason call) keeps the cancel atomic and saves a
-- round-trip.
UPDATE runs
SET status = 'canceled',
    finished_at = COALESCE(finished_at, NOW()),
    queue_reason = NULL
WHERE id = $1 AND status IN ('queued', 'running')
RETURNING id;

-- name: GetJobRunForCancel :one
-- Thin row used by the job-scoped cancel handler. Returns the
-- structural pointers cancel needs (run_id, stage_run_id) plus the
-- decision inputs (status, agent_id) — `running` jobs are signaled
-- via gRPC (handler dispatches CancelJob), `queued` jobs flip
-- directly here and feed cascadeAfterJobCompletion. Distinguishes
-- "not found" from "already terminal" via pgx.ErrNoRows vs the
-- status check in the caller.
--
-- `FOR UPDATE` serialises against the scheduler's
-- ClaimJobForDispatch (which writes status='running' + agent_id
-- inside its own tx). Without the lock, a job_run that's queued
-- when we SELECT can be dispatched by the time CancelQueuedJobRun
-- UPDATEs — the UPDATE misses the predicate, we return 409
-- "already terminal" while the job is actually running. With the
-- lock, exactly one of (cancel commits / dispatch commits) wins;
-- the loser sees the post-commit state and routes correctly.
SELECT id, run_id, stage_run_id, name, status, agent_id, attempt
FROM job_runs
WHERE id = $1
FOR UPDATE;

-- name: CancelQueuedJobRun :one
-- Flips a single job_run to 'canceled' ONLY when it's still queued.
-- `running` jobs go through the agent's gRPC CancelJob → JobResult
-- path so the audit trail records the actual stop time. Returns the
-- row id so the caller can tell whether the update happened (the
-- predicate could miss in a race with the scheduler dispatch path).
--
-- We DON'T pre-fill exit_code or error here — those columns are
-- agent-driven on running cancels, and for a queued cancel the
-- absence of an exit_code is the honest signal ("never started").
UPDATE job_runs
SET status      = 'canceled',
    finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status = 'queued'
RETURNING id;

-- name: GetLatestModificationForPipeline :one
-- Most recent modification across any material attached to a
-- pipeline. Powers "trigger latest" for manual runs. Ordered by
-- detected_at so the newest webhook delivery wins even when the
-- committer timestamp is older (rebases, fast-forwards of older
-- commits).
SELECT m.id, m.material_id, m.revision, m.branch
FROM modifications m
JOIN materials mat ON mat.id = m.material_id
WHERE mat.pipeline_id = $1
ORDER BY m.detected_at DESC
LIMIT 1;
