-- name: OtherRunningRunForPipeline :one
-- Returns the run_id of an in-flight predecessor blocking the
-- pipeline's serial-concurrency gate, or pgx.ErrNoRows if none.
-- Used by the scheduler for two things on one query: (1) the busy
-- decision (leave queued vs proceed), and (2) the predecessor id to
-- stamp on runs.queue_reason so the UI can render "waiting on #N".
--
-- Replaces the prior boolean-returning OtherRunningRunExistsForPipeline
-- — the boolean was load-bearing for the decision alone, but the id
-- is needed for the operator-visibility surface (issue #4 path #2).
-- Excluded self so a re-entrant tick (scheduler evaluates the same
-- run twice) doesn't see itself as a blocker. LIMIT 1 is fine: any
-- one blocking run is enough to leave queued; we don't need the
-- full set.
SELECT id
FROM runs
WHERE pipeline_id = $1
  AND status = 'running'
  AND id <> $2
LIMIT 1;

-- name: ListAgentsForRun :many
-- Every distinct agent that ran (or is running) at least one job
-- of the given run, FILTERED to engines that can actually do the
-- cleanup work — k8s today. Used by the run-terminal
-- CleanupRunServices dispatch.
--
-- Why the engine filter:
--   - A mixed-engine run (job 1 on k8s, job 2 on docker) puts
--     BOTH agents on the unfiltered list. The docker agent's
--     CleanupRunServices is a no-op that returns
--     success-with-0-deleted, which the server's ok-counter
--     would mistakenly count as a successful cleanup,
--     masking the fact that the disconnected k8s agent's pods
--     never got reaped.
--   - Filtering at the SQL layer means the ok-count surfaces
--     ONLY engines that COULD have done useful work, so a "0
--     successful dispatches" log line is operationally
--     trustworthy.
--
-- agents.engine='' (empty / legacy / pre-v0.4.35) passes the
-- filter defensively — a rolling upgrade window where old agents
-- haven't been redeployed yet still gets best-effort cleanup
-- coverage; the value is overwritten on the next Register.
SELECT DISTINCT j.agent_id
FROM job_runs j
JOIN agents a ON a.id = j.agent_id
WHERE j.run_id = $1
  AND j.agent_id IS NOT NULL
  AND (a.engine = '' OR a.engine = 'kubernetes');

-- name: RunHasServices :one
-- Snapshot read: was the run created with a non-empty `Services`
-- block in its pipeline definition? Persisted on insert
-- (migration 00036) rather than computed live from
-- pipelines.definition, so an ApplyProject that adds/removes
-- services mid-run doesn't lie to the cleanup cascade. Defaults
-- to false when the run row is missing (cleanup skipped, safer
-- than fail-open here — operator can run manual sweep if a stale
-- leak surfaces).
SELECT has_services FROM runs WHERE id = $1;

-- name: SetRunQueueReason :exec
-- Sets runs.queue_reason on a still-queued run. Callers stamp this
-- AFTER the busy decision so the field reflects the latest tick's
-- reasoning. A run that flipped to a terminal status between the
-- scheduler's read and this write would write to a stale row;
-- the `status='queued'` guard turns that into a no-op rather than
-- planting a confusing message on a finished run.
UPDATE runs
SET queue_reason = $2
WHERE id = $1
  AND status = 'queued';

-- name: ClearRunQueueReason :exec
-- Clears queue_reason. Called by the scheduler when a run transitions
-- to running (predecessor finished, run is dispatchable). Also called
-- by terminal-transition paths so a canceled-while-queued run doesn't
-- carry a stale "waiting on #N" message in the runs list.
UPDATE runs
SET queue_reason = NULL
WHERE id = $1
  AND queue_reason IS NOT NULL;

-- name: ListDispatchableJobs :many
-- Returns queued jobs in the lowest-ordinal stage that still has queued or
-- running work. The scheduler does needs-satisfaction checking in Go so the
-- query stays readable; the stage gate is the only SQL-level constraint.
WITH active_stage AS (
    SELECT MIN(s.ordinal) AS ordinal
    FROM stage_runs s
    WHERE s.run_id = $1 AND s.status IN ('queued', 'running')
)
SELECT j.id, j.run_id, j.stage_run_id, j.name, j.matrix_key, j.image, j.status, j.needs, j.attempt
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
--
-- `attempt` flows back so the scheduler can RecordAssignment on the
-- target session — the result handler then validates the (agent,
-- attempt) snapshot to refuse stale results from revoked sessions
-- whose job got redispatched to the same agent_id on a higher
-- attempt. See sessions.Session.RecordAssignment.
UPDATE job_runs
SET status = 'running', agent_id = $2, started_at = NOW()
WHERE id = $1 AND status = 'queued' AND agent_id IS NULL
RETURNING id, run_id, stage_run_id, name, matrix_key, image, status, agent_id, attempt;

-- name: UnassignJob :one
-- Rolls back an AssignJob whose Dispatch failed downstream (busy
-- session queue, session vanished between AssignJob commit and the
-- gRPC send). Snapshot-validating CAS — only undoes the row if it's
-- still (running, $agentID, $attempt), matching exactly what we
-- just claimed. Anything else means a reaper / fence already moved
-- the row out from under us and our undo would clobber legitimate
-- state. Returns the run_id so the caller can fire a NOTIFY to
-- nudge the scheduler back into the dispatch loop.
--
-- The row goes back to (queued, NULL, attempt unchanged) — we do
-- NOT bump attempt here. The attempt counter exists to detect
-- crashes mid-execution; a dispatch failure that never reached the
-- agent doesn't count as an attempt.
-- cancel_requested_at = NULL: defensive clear in case a cancel
-- landed in the AssignJob→DispatchAssignment window (operator
-- raced the scheduler). The row is going back to 'queued' with
-- agent_id NULL — there's no agent to replay against, and the
-- next AssignJob may pick a different agent entirely; carrying
-- the stamp forward would let ListPendingCancelsForAgent honor
-- it against an agent that never received the cancel intent.
--
-- logs_archive_uri / logs_archived_at = NULL: an AssignJob that
-- bounced into UnassignJob almost never had time to archive logs
-- (the archiver fires from terminal status, not the brief
-- running window before Dispatch failed), but if a redispatch
-- picks the same row up later, reads against the row would
-- otherwise see a stale archive pointer. Defensive mirror of
-- the RerunJob reset list.
UPDATE job_runs
SET status = 'queued',
    agent_id = NULL,
    started_at = NULL,
    cancel_requested_at = NULL,
    logs_archive_uri = NULL,
    logs_archived_at = NULL
WHERE id = $1
  AND status = 'running'
  AND agent_id = $2
  AND attempt = @expected_attempt::int
RETURNING id, run_id;

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

-- name: ListJobOutputsForRun :many
-- Reads (name, matrix_key, outputs) for every job_run in the given
-- run whose name appears in @names. Used by the scheduler during
-- dispatch to resolve `${{ needs.<job>.outputs.<key> }}` refs in a
-- downstream job's `with:` / `env:` / `script:` (issue #10).
--
-- Outputs are NOT NULL DEFAULT '{}' on the column, so a job that
-- ran without writing outputs returns an empty map — the
-- substitution layer surfaces "key missing" as a hard error
-- (operator referenced a key the upstream never produced) rather
-- than silently substituting empty.
--
-- Why scoped by run_id + name list (not just run_id): a typical
-- needs: list is 1–3 names, the run has 10–50 job_runs. Filtering
-- at SQL avoids dragging unrelated rows + their JSONB payloads
-- across the wire. matrix_key returns so the scheduler's
-- groupNeedsOutputs can route each row to the right table:
-- matrix_key='' rows fold into NeedsOutputs (bare-ref path),
-- matrix_key!='' rows go into MatrixNeedsOutputs (issue #21
-- selector path).
SELECT name, matrix_key, status, outputs
FROM job_runs
WHERE run_id = $1
  AND name = ANY(@names::text[]);

-- name: GetRunForDispatch :one
-- project_notifications tags along so the dispatcher can resolve
-- synth notification jobs that inherited their spec from the
-- project (pipeline didn't declare `notifications:`). Single
-- round-trip keeps the dispatch hot path tight.
--
-- r.cause + r.cause_detail come along because scheduler/civars.go
-- materialises CI_CAUSE + CI_PULL_REQUEST_* env vars from them.
-- Adding the columns here costs one extra row width on a hot path
-- query that already loads the JSONB definition — negligible vs.
-- the round trip we'd otherwise need to fetch them separately.
SELECT r.id, r.pipeline_id, p.project_id, r.counter, r.status, r.revisions,
       r.cause, r.cause_detail,
       p.definition, p.config_path,
       pr.notifications AS project_notifications,
       pr.slug AS project_slug
FROM runs r
JOIN pipelines p ON p.id = r.pipeline_id
JOIN projects pr ON pr.id = p.project_id
WHERE r.id = $1
LIMIT 1;
