-- +goose Up
-- +goose StatementBegin

-- Durable delivery of a superseded run's EXTERNAL effects (#97 pt.5d). Supersede
-- terminalizes the run in-tx and NOTIFYs run_superseded; the scheduler's listener
-- fires CancelJob frames + service cleanup + GitHub check close + audit. But
-- LISTEN/NOTIFY is neither durable nor single-consumer: a channel-full drop or a
-- scheduler restart loses the prompt effects, and every replica's listener receives
-- the event (duplicate audit). This adds a claim/complete marker so exactly one
-- worker fires the effects, a periodic replay recovers missed ones, and a crashed
-- claim is retried after a lease instead of being lost forever.
--
--   supersede_effects_claimed_at — a worker took the effects; a claim older than the
--     lease is reclaimable (the prior claimer crashed mid-effects).
--   supersede_effects_at         — effects COMPLETED. Set only after frames/cleanup/
--     check/audit all ran, so the replay retries until they truly landed.
ALTER TABLE runs ADD COLUMN supersede_effects_claimed_at TIMESTAMPTZ;
ALTER TABLE runs ADD COLUMN supersede_effects_at         TIMESTAMPTZ;

-- Backfill any already-superseded runs as done so enabling the replay doesn't
-- re-fire effects for history. (0 rows in practice — supersede ships in this train
-- and no run carries superseded_by yet — but defensive against dev/test data.)
UPDATE runs SET supersede_effects_at = COALESCE(finished_at, NOW())
  WHERE superseded_by IS NOT NULL;

-- Replay lookup: superseded runs whose effects haven't completed (missed NOTIFY, or
-- a claim past its lease). Partial index keeps the periodic scan index-only + tiny.
CREATE INDEX runs_supersede_effects_pending_idx ON runs (finished_at)
  WHERE superseded_by IS NOT NULL AND supersede_effects_at IS NULL;

-- Once-guarantee for the run.superseded audit: at most one row per victim run, so a
-- replica race or a lease-expiry replay (which re-runs the idempotent frames/cleanup/
-- check) can't duplicate the audit. The emit uses ON CONFLICT DO NOTHING on this.
CREATE UNIQUE INDEX audit_run_superseded_once ON audit_events (target_id)
  WHERE action = 'run.superseded';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS audit_run_superseded_once;
DROP INDEX IF EXISTS runs_supersede_effects_pending_idx;
ALTER TABLE runs DROP COLUMN IF EXISTS supersede_effects_at;
ALTER TABLE runs DROP COLUMN IF EXISTS supersede_effects_claimed_at;
-- +goose StatementEnd
