-- name: UpdateAgentLastSeen :exec
-- Called from the gRPC heartbeat handler so the reaper can tell which agents
-- are still alive. Tiny write per heartbeat (default cadence 30s).
UPDATE agents SET last_seen_at = NOW() WHERE id = $1;

-- name: ListStaleRunningJobs :many
-- Running jobs the reaper should reclaim. Two distinct categories live
-- under the same SELECT so a single sweep tick handles both:
--
--   1) Has agent, agent is unhealthy: status='offline', missing
--      last_seen_at, or last_seen_at older than @staleness. Original
--      reaper case — agent process died and we wait the heartbeat
--      grace window before declaring its jobs gone.
--   2) Has NO agent (agent_id IS NULL) and the row has been sitting
--      that way past @staleness. This state shouldn't be reachable
--      via normal paths (AssignJob is atomic on both fields, and
--      ReclaimJobForRetry flips status to 'queued' in the same UPDATE
--      that NULLs the agent), but it surfaces from:
--        - Manual DB intervention to scrub an agent without flipping
--          status.
--        - Future code regression that splits the agent/status update.
--        - Partial migration that nulled agent_id mid-flight.
--      The INNER JOIN variant this replaced was invisible to such
--      orphans — they'd sit 'running' forever, blocking serial
--      pipelines indefinitely. Issue #4 walks the operator-visibility
--      consequences.
--
-- agent_status / last_seen_at come back NULL for category (2); the
-- reaper-Go side doesn't act on those fields beyond logging.
--
-- agent_session_generation is the per-agent monotonic epoch counter
-- snapshotted at SELECT time. The reaper-Go side passes this back to
-- SessionStore.FenceStaleSession so a successor Register that bumped
-- the counter between SELECT and fence (race that previously could
-- revoke a freshly-online healthy session) is correctly skipped.
-- COALESCE to 0 for category (2) so the Go side gets a stable int —
-- the NULL-agent fence path uses agent_id == uuid.Nil as its skip
-- predicate anyway.
SELECT j.id, j.run_id, j.stage_run_id, j.name, j.attempt, j.agent_id,
       a.status AS agent_status, a.last_seen_at,
       COALESCE(a.session_generation, 0)::bigint AS agent_session_generation
FROM job_runs j
LEFT JOIN agents a ON a.id = j.agent_id
WHERE j.status = 'running'
  -- Server-managed native deploy (ADR-0001): a `deploy:` job with a registered
  -- target runs with NO agent (the server drives the sync + watch), so it would
  -- otherwise trip Category 2 below. While a deploy_watch is alive the WATCHER owns
  -- this job and completes it on convergence — the reaper must not reap it as an
  -- orphan. When the watch terminalizes (or, as a recovery path, ever vanishes with
  -- the job still running) this guard lifts and the reaper sees the job again.
  AND NOT EXISTS (
    SELECT 1 FROM deploy_watches dw
    JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
    WHERE dr.job_run_id = j.id
  )
  AND (
    -- Category 1: has agent + agent is stale.
    (a.id IS NOT NULL AND (
        a.status = 'offline'
        OR a.last_seen_at IS NULL
        OR a.last_seen_at < NOW() - @staleness::INTERVAL
    ))
    -- Category 2: no agent + job itself looks orphaned.
    -- `started_at IS NULL` AND status='running' is an impossible
    -- combination via normal paths (AssignJob sets both atomically;
    -- ReclaimJobForRetry flips to 'queued' atomically); when we
    -- see it, the row was corrupted by manual intervention / failed
    -- migration / future-code regression and should be reclaimed
    -- IMMEDIATELY — waiting out a staleness window for an impossible
    -- state buys nothing and can leave a serial-concurrency block in
    -- place forever. For NULL-agent rows that DO have a started_at,
    -- the staleness window still applies.
    OR (a.id IS NULL AND (j.started_at IS NULL OR j.started_at < NOW() - @staleness::INTERVAL))
  );

-- name: ListRunningJobsForAgent :many
-- Every running job currently assigned to a given agent_id. The
-- register-fence path uses this: when an agent re-registers (= the
-- prior process is gone), every still-'running' row attributed to it
-- is by definition orphaned and must be reclaimed before we accept
-- new assignments for the new session. This is the primary fix for
-- "agent restarts → job stuck running forever → reaper skips because
-- last_seen_at is fresh" (issue #4).
--
-- Returned columns mirror ListStaleRunningJobs so the reaper-Go
-- side can reuse the same ReclaimResult shape.
--
-- cancel_requested_at IS NULL guard: rows with a cancel intent
-- stamped don't belong to this reclaim path — they belong to the
-- agent_service.Register replay path
-- (ListPendingCancelsForAgent), which honors the cancel as a
-- terminal 'canceled' instead of dropping it back to 'queued' as
-- a retry. Without the guard, the reclaim wins the race and the
-- operator's cancel intent becomes a generic requeue; on hits
-- past max attempts it becomes a failed (not canceled) row,
-- mis-attributing the operator's deliberate stop as a process
-- crash.
SELECT j.id, j.run_id, j.stage_run_id, j.name, j.attempt, j.agent_id
FROM job_runs j
WHERE j.status = 'running'
  AND j.agent_id = $1
  AND j.cancel_requested_at IS NULL;

-- name: ReclaimJobForRetry :one
-- Flips a running job back to queued, IF it is still running, still
-- under the retry cap, AND its (agent_id, attempt) STILL matches the
-- snapshot the caller observed when it decided the job was stale.
--
-- Snapshot match is load-bearing: between ListStaleRunningJobs and
-- this UPDATE, a concurrent reaper / register-fence / rerun could
-- have flipped status=queued → scheduler dispatched → AssignJob set
-- (running, NEW_AGENT, attempt+1). Without snapshot validation, this
-- UPDATE would happily requeue a healthy job actively running on
-- the new agent, kicking off a cascade of unnecessary retries that
-- the operator perceives as "jobs randomly re-running for no reason".
--
-- `agent_id IS NOT DISTINCT FROM` honours NULL equality — the
-- NULL-agent reaper category sees `expected_agent_id` as NULL and
-- only requeues rows that still have NULL there.
--
-- ErrNoRows signals the caller should take a different code path
-- (failed-at-max, already-reclaimed, raced-out-of-window).
-- cancel_requested_at = NULL: defensive clear, mirrored from
-- RerunJob's reset list. ListRunningJobsForAgent already excludes
-- rows with cancel_requested_at IS NOT NULL from the
-- register-fence reclaim, so a stamped row should NEVER reach
-- this UPDATE — but the clear is cheap and closes the door on
-- an in-flight Phase 0 reaper losing a CAS race against this
-- one, which would otherwise leave the row queued with a stale
-- stamp and re-trigger the replay path on the next AssignJob.
--
-- logs_archive_uri / logs_archived_at = NULL: the prior attempt's
-- archive points at a GCS object holding the OLD run's logs;
-- without clearing, the reads.go cold-archive fallback would
-- surface those old logs in the UI for the new attempt. Mirrored
-- from RerunJob's reset for the same reason.
UPDATE job_runs
SET status = 'queued', agent_id = NULL, started_at = NULL, finished_at = NULL,
    exit_code = NULL, error = NULL, cancel_requested_at = NULL,
    logs_archive_uri = NULL, logs_archived_at = NULL,
    attempt = attempt + 1
WHERE id = $1
  AND status = 'running'
  AND attempt < @max_attempts::int
  AND attempt = @expected_attempt::int
  AND agent_id IS NOT DISTINCT FROM @expected_agent_id::uuid
RETURNING id, run_id, stage_run_id, name, attempt;

-- name: FailStaleJobAtMax :one
-- Cap-exceeded path for the reaper / fence: flips a still-running
-- row to 'failed' with a fixed error message, IF the snapshot the
-- caller saw still matches. Snapshot validation has the same role
-- as in ReclaimJobForRetry — a concurrent rerun or redispatch could
-- have moved this row to a healthy state on another agent, and
-- failing it on our stale read would corrupt the run.
--
-- We don't go through CompleteJobRun because that UPDATE is
-- intentionally permissive (accepts both queued+running so a
-- dispatch-time fail-from-queued path works), and adding the
-- snapshot check there would ripple across every result-handler
-- call site. The caller (FailJobIfStale in store/sweeper.go) wraps
-- this query + the existing cascadeAfterJobCompletion helper in
-- one transaction so the stage/run promotion stays identical to
-- the normal terminal path.
--
-- Return columns mirror CompleteJobRun so the Go caller can
-- reuse the same JobCompletion struct + cascade helper.
UPDATE job_runs
SET status = 'failed', finished_at = NOW(), exit_code = -1, error = @reason::text
WHERE id = $1
  AND status = 'running'
  AND attempt = @expected_attempt::int
  AND agent_id IS NOT DISTINCT FROM @expected_agent_id::uuid
RETURNING id, run_id, stage_run_id, agent_id, name, started_at, finished_at;

-- name: DeleteLogLinesByJob :exec
-- Called after a successful reclaim so the retry starts with a clean log
-- window. Loses the old attempt's output — acceptable MVP trade for not
-- growing the schema to carry per-attempt log namespacing.
DELETE FROM log_lines WHERE job_run_id = $1;

-- name: GetJobAttempt :one
-- Used by the reaper to disambiguate "at max attempts" from "already
-- handled" when ReclaimJobForRetry returned no rows.
SELECT id, status, attempt FROM job_runs WHERE id = $1 LIMIT 1;
