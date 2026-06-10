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
SELECT id, run_id, stage_run_id, name, status, agent_id, attempt, cancel_requested_at
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

-- name: StampCancelRequestedAtForRun :many
-- Batch variant of StampCancelRequestedAt used by the run-scoped
-- cancel path. Stamps cancel_requested_at on EVERY running job
-- belonging to the run that hasn't been stamped yet. The handler
-- then attempts gRPC dispatch per row; any dispatch that lands in
-- the Revoke→Register race window is rescued by the same replay
-- + reaper paths that cover the job-scoped cancel.
--
-- COALESCE preserves the FIRST stamp's at-time across re-cancel
-- attempts (the same idempotency as the single-row variant).
-- Returns (id, agent_id) per stamped row so the handler can
-- correlate Dispatch failures with their owning agent.
UPDATE job_runs
SET cancel_requested_at = COALESCE(cancel_requested_at, NOW())
WHERE run_id = $1
  AND status = 'running'
  AND agent_id IS NOT NULL
RETURNING id, agent_id;

-- name: StampCancelRequestedAt :one
-- Persists the cancel INTENT on a running job_run BEFORE the
-- handler attempts the gRPC dispatch. Decouples the cancel UX
-- from "is the agent's session alive in this instant" — even if
-- Dispatch fails because of a Revoke→Register race (the agent
-- pod is in flux), the intent survives. When the agent's new
-- session comes up it calls ListPendingCancelsForAgent and
-- honors the cancel; if it never comes back, the reaper
-- finalises the row via ReclaimPendingCancelsForOfflineAgent.
--
-- Idempotent on the timestamp: COALESCE keeps the FIRST cancel
-- request's at-time so the audit trail matches the first click
-- (a re-click that lands in the brief window between dispatch
-- attempts shouldn't reset the requested-at clock).
--
-- Predicate guards: only stamp when the row is STILL running
-- AND has an agent_id pinned (no-op on rows that finished
-- between the cancel handler's read and this write). The
-- handler ran GetJobRunForCancel under FOR UPDATE in the same
-- tx, so by definition the row was running at SELECT time —
-- but a result handler in another tx can have committed in
-- between if the cancel handler is using a separate tx. The
-- guards keep us honest in either calling shape.
UPDATE job_runs
SET cancel_requested_at = COALESCE(cancel_requested_at, NOW())
WHERE id = $1
  AND status = 'running'
  AND agent_id IS NOT NULL
RETURNING id, agent_id, cancel_requested_at;

-- name: ListPendingCancelsForAgent :many
-- The agent calls this right after Register + Connect lands so
-- it picks up any cancels that were requested while its session
-- was being recycled. Returns (job_run_id, run_id) pairs the
-- agent can issue local-cancel-equivalent handling on. The
-- agent then sends the usual JobResult(status=canceled) to
-- close out each row server-side — the existing result handler
-- finalises the status, and the cancel_requested_at column
-- becomes operationally redundant on that row (kept for the
-- audit trail's at-time).
--
-- Hits the partial index job_runs_pending_cancel_by_agent_idx;
-- index-only on the WHERE clause makes this a sub-millisecond
-- lookup even on a job_runs table in the millions.
SELECT id, run_id
FROM job_runs
WHERE agent_id = $1
  AND status = 'running'
  AND cancel_requested_at IS NOT NULL;

-- name: ReclaimPendingCancelsForOfflineAgent :many
-- The reaper's path for cancels that never reached an agent
-- because the agent went offline and stayed offline past the
-- cancel grace window. Finalises every still-running row that
-- belongs to an offline agent AND was cancel-requested as
-- 'canceled' with finished_at=NOW().
--
-- Why this lives in the reaper (not a hot path):
--
--   * The hot path (cancel handler) does not block on agent
--     liveness — it stamps cancel_requested_at and returns
--     202 immediately. The DB row stays 'running' from the
--     handler's perspective; the reaper does the finalising
--     when it's clear the agent isn't coming back.
--
--   * Latency of cancel finalisation = reaper tick interval +
--     offline-grace window. Both are operator-tunable; default
--     is ~5min total. Acceptable for a "we tried, agent gone"
--     case; the operator-visible status is 'canceling' in the
--     meantime via the cancel_requested_at column.
--
-- Liveness predicate mirrors the reaper's main path
-- (ListStaleRunningJobs in sweeper.sql) so a heartbeat-stale
-- agent that's still marked online doesn't accumulate
-- cancel-pending rows forever. Three failure shapes catch
-- everything except a perfectly healthy agent the replay path
-- still owns:
--
--   1. a.status = 'offline'  — explicit liveness flip; the
--      Connect-defer or the reaper has marked the row offline.
--   2. a.last_seen_at IS NULL — agent never sent a heartbeat
--      since this server started; either fresh-DB state or a
--      truly dead row.
--   3. a.last_seen_at < NOW() - grace — heartbeat stopped past
--      the grace window. agents.status may still be 'online'
--      if the offline-mark never fired (network partition;
--      Connect-defer crashed), but the heartbeat signal is
--      gone for real.
--
-- A perfectly healthy agent (status=online AND last_seen_at
-- recent) is skipped — the replay path is still expected to
-- land the cancel on its next Connect frame.
UPDATE job_runs jr
SET status      = 'canceled',
    finished_at = COALESCE(finished_at, NOW())
FROM agents a
WHERE jr.agent_id = a.id
  AND jr.status = 'running'
  AND jr.cancel_requested_at IS NOT NULL
  AND (
        a.status = 'offline'
        OR a.last_seen_at IS NULL
        OR a.last_seen_at < NOW() - sqlc.arg(grace_interval)::INTERVAL
      )
RETURNING jr.id, jr.run_id, jr.stage_run_id, jr.agent_id, jr.cancel_requested_at, jr.name;

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
