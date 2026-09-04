-- +goose NO TRANSACTION

-- +goose Up

-- Durable delivery for generic run-terminal effects (#211). Normal completion,
-- API cancel, approval expiry, queued-job cancel, reaper finalization, and native
-- watch finalization can all be the transaction that flips a run to a terminal
-- state. The prompt callers may still fire best-effort cleanup/check updates, but
-- LISTEN/NOTIFY is not durable and those callers are not all wired to every effect.
--
--   terminal_effects_claimed_at — a worker claimed the terminal effects; a claim
--     older than the lease is reclaimable after a crash.
--   terminal_effects_at         — durable effects resolved. GitHub check updates
--     stay best-effort; service cleanup for runs that declared services gates this
--     marker so exposed workloads are retried until a k8s-capable agent receives
--     the cleanup frame.
--   terminal_effects_required   — true only for terminalizations that happened
--     after this migration. Existing historical terminal rows default false, so the
--     upgrade avoids a full-table UPDATE/WAL storm and does not re-fire old effects.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS terminal_effects_required   BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS terminal_effects_claimed_at TIMESTAMPTZ;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS terminal_effects_at         TIMESTAMPTZ;

-- Replay lookup for terminal runs whose generic effects did not complete. Exclude
-- supersede and merge_group-destroyed cancels: they have cause-specific workers with
-- different semantics (supersede audit, merge-group check suppression).
CREATE INDEX CONCURRENTLY IF NOT EXISTS runs_terminal_effects_pending_idx ON runs (finished_at, id)
  WHERE terminal_effects_required = true
    AND status NOT IN ('queued', 'running')
    AND superseded_by IS NULL
    AND terminal_effects_at IS NULL
    AND NOT (
        cause = 'merge_group'
        AND COALESCE(cancel_reason, '') LIKE 'github merge_group destroyed%'
    );

COMMENT ON COLUMN runs.terminal_effects_claimed_at IS
    'Lease claim for generic terminal run effects (GitHub completion + service cleanup).';
COMMENT ON COLUMN runs.terminal_effects_at IS
    'Set once generic terminal run effects have resolved; service cleanup is durable, GitHub is best-effort.';
COMMENT ON COLUMN runs.terminal_effects_required IS
    'True when a post-migration terminal run still needs generic terminal effects.';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_terminal_effects_pending_idx;
ALTER TABLE runs DROP COLUMN IF EXISTS terminal_effects_at;
ALTER TABLE runs DROP COLUMN IF EXISTS terminal_effects_claimed_at;
ALTER TABLE runs DROP COLUMN IF EXISTS terminal_effects_required;
