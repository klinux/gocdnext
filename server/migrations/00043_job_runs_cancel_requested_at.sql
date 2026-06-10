-- +goose Up
-- +goose StatementBegin

-- cancel_requested_at — when an operator (or an automated rule)
-- has requested cancel for this job_run. Separate from `status`
-- so the existing terminal-vs-running enum keeps its semantics:
-- a running job with cancel_requested_at IS NOT NULL is in the
-- "canceling" intermediate state — the agent hasn't yet acked
-- via JobResult.
--
-- Why a column, not a new enum value:
--
--   * Adding 'canceling' to the status enum touches every
--     consumer of `status` (UI badges, scheduler predicates,
--     reaper SQL, audit serialisation). A column is a strict
--     ADD — readers that don't look at it see the row as
--     'running' (which it still is at the workload level until
--     the agent confirms termination).
--   * Reversible without a down migration. The reaper path
--     plus the agent-side honor-on-connect logic both no-op
--     when the column is absent on a downgrade (defensive
--     pre-flight check at the gateway layer).
--
-- Why TIMESTAMPTZ (not BOOLEAN):
--
--   * Lets the reaper bound "queued for cancel for too long" —
--     after N seconds without agent ack on a row whose agent is
--     offline, the reaper finalises the row as `canceled`. A
--     boolean would force the reaper to consult audit_events
--     for the actual timestamp, doubling the reaper's read
--     work per scan.
--   * Audit trail keeps the canonical (who/when) tuple in
--     audit_events; this column is operationally redundant
--     for "when" but eliminates the audit JOIN.
ALTER TABLE job_runs
    ADD COLUMN cancel_requested_at TIMESTAMPTZ;

-- Partial index pinned to the agent for the reaper's
-- ReclaimPendingCancels sweep and the agent-side
-- ListPendingCancelsForAgent lookup. Both queries are gated by
-- WHERE cancel_requested_at IS NOT NULL AND status = 'running'
-- AND agent_id = $1 — a partial index over only the actively
-- canceling rows keeps the storage cost negligible (most rows
-- are NULL) while making both lookups index-only scans.
CREATE INDEX job_runs_pending_cancel_by_agent_idx
    ON job_runs(agent_id)
    WHERE cancel_requested_at IS NOT NULL
      AND status = 'running'
      AND agent_id IS NOT NULL;

-- +goose StatementEnd
