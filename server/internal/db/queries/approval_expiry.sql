-- name: ListPendingApprovalGates :many
-- Candidate gates for the approval expirer: every gate still parked in
-- `awaiting_approval` whose wait already exceeds the SHORTEST window that
-- could possibly apply (domain.ApprovalTimeoutMin). The expirer then resolves
-- each candidate's effective window from the run's pipeline definition —
-- per-gate `timeout:` beats the server default, and `never` opts out — because
-- the window lives in JSON, not in a column.
--
-- Deliberately NOT pre-filtered by the server default: a gate may declare a
-- window far shorter than the fleet default, and filtering on the default
-- would never surface it.
--
-- Cost: driven by the partial index idx_job_runs_awaiting_approval
-- (awaiting_since ASC) WHERE status = 'awaiting_approval' — migration 00017,
-- which matches this predicate exactly. The join to runs is a PK lookup per
-- row. The row set is the pending-approval queue, i.e. bounded by how many
-- decisions humans owe, not by run history — hundreds at the pathological end.
-- The projection is deliberately narrow (no definition JSON): the expirer
-- fetches definitions only for candidates it must resolve, deduped by run.
--
-- KEYSET-PAGINATED, and that is a correctness requirement, not an optimisation.
-- Whether a candidate actually expired is only knowable in Go (the window lives
-- in the definition JSON), so a plain `ORDER BY awaiting_since ASC LIMIT n`
-- truncates the set BEFORE that decision: n older gates that are `never` or
-- merely still-inside-a-7-day-window would hide a newer gate with
-- `timeout: 5m` from every sweep, forever. Paging past them is the only way the
-- filter gets to see everything.
--
-- (cursor_since, cursor_id) is the keyset cursor — the id breaks ties because
-- awaiting_since is NOT unique: every gate of a run is stamped in one
-- transaction, so siblings share a timestamp to the microsecond, and a
-- timestamp-only cursor would either skip or re-serve them forever. The caller
-- resumes the cursor ACROSS sweeps and wraps at the end of the queue, so a
-- bounded per-sweep scan still visits every gate eventually.
--
-- Written as the expanded (a > c OR (a = c AND b > d)) rather than the row
-- comparison (a, b) > (c, d): sqlc infers a row comparison's later elements
-- from the type of the first and mistypes the id cursor as a timestamptz.
-- Semantically identical, and still sargable on awaiting_since.
SELECT j.id, j.run_id, j.name, j.awaiting_since, r.pipeline_id, r.counter
FROM job_runs j
JOIN runs r ON r.id = j.run_id
WHERE j.approval_gate = true
  AND j.status = 'awaiting_approval'
  AND j.awaiting_since IS NOT NULL
  AND j.awaiting_since < sqlc.arg(older_than)::timestamptz
  AND (j.awaiting_since > sqlc.arg(cursor_since)::timestamptz
       OR (j.awaiting_since = sqlc.arg(cursor_since)::timestamptz
           AND j.id > sqlc.arg(cursor_id)::uuid))
  AND r.status IN ('queued', 'running')
ORDER BY j.awaiting_since ASC, j.id ASC
LIMIT sqlc.arg(page_size);

-- name: MarkApprovalGateExpired :one
-- Stamp the gate row with the expiry decision so the UI and the audit trail
-- can say WHY it died rather than showing a bare cancel. Guarded on
-- status = 'awaiting_approval', which doubles as the race check: a human who
-- approved or rejected between the candidate scan and this write moves the row
-- out of that status, this returns no rows, and the expirer abandons the whole
-- expiry instead of cancelling a run someone just decided on.
--
-- decided_by stays NULL — nobody decided. The `expired` decision value is what
-- distinguishes this from an approve/reject; status is left to the shared
-- cancel cascade (CancelQueuedJobsInRun already covers awaiting_approval rows).
--
-- TOCTOU guard (#208): relate the row to the run AND the exact awaiting_since
-- the candidate scan observed. A rerun that re-parks the gate (re-stamping
-- awaiting_since to a fresh instant) or a human deciding between the scan and
-- this write moves the row off the (run_id, awaiting_since) the expiry was
-- authorised for, so this returns no rows and the whole expiry aborts instead
-- of cancelling a run under a window that was just reset.
UPDATE job_runs
SET decision = 'expired', decided_at = NOW()
WHERE id = $1
  AND run_id = @run_id
  AND awaiting_since = @awaiting_since
  AND approval_gate = true
  AND status = 'awaiting_approval'
RETURNING id;

-- name: ExpireApprovalRun :one
-- Flip the run of an expired gate to canceled, carrying a human-readable
-- reason. Same shape and guard as CancelActiveRun/SupersedeRun (idempotent —
-- a second call on a terminal run returns no rows).
--
-- CANCELED, NOT FAILED, and that is load-bearing: the dashboard computes
-- success rate as success/(success+failed) with canceled excluded
-- (queries/dashboard.sql), so reporting an abandoned gate as a failure would
-- silently degrade every pipeline's success metric. Nobody's build broke —
-- nobody decided.
--
-- cancel_reason cites the window that elapsed, never an approver identity or
-- any ref value. service_generation is RETURNED in this same UPDATE (#97) so
-- the service cleanup carries it as max_generation and a later rerun that
-- revives the run into a higher generation keeps its fresh pods.
UPDATE runs
SET status = 'canceled',
    finished_at = COALESCE(finished_at, NOW()),
    queue_reason = NULL,
    cancel_reason = $2
WHERE id = $1 AND status IN ('queued', 'running')
RETURNING id, service_generation;
