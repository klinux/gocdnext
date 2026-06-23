-- +goose Up
-- +goose StatementBegin
-- completed tracks whether the run→check link's CURRENT GitHub check run has
-- already been completed. GitHub treats a check run's completed_at as set-once:
-- PATCHing a completed check back to in_progress does NOT cleanly reopen it —
-- the PR keeps showing the prior conclusion for the whole rerun, so a rerun
-- looks like it "only reports at the end". On a rerun we therefore create a
-- FRESH check run instead of reusing a completed one, and this flag is the
-- deterministic signal for that decision: reset to FALSE on create/reopen, set
-- TRUE on completion. The per-run advisory lock serialises concurrent
-- job-reruns so only the first recreates; the rest see FALSE and reuse.
ALTER TABLE github_check_runs ADD COLUMN completed BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd

-- +goose StatementBegin
-- Backfill: a check whose run already reached a terminal state was completed on
-- GitHub too. Without this, the first post-deploy rerun of an already-finished
-- run would take the OLD path (PATCH a completed check back to in_progress) and
-- exhibit the very bug this column fixes. Active runs (queued/running) correctly
-- keep the FALSE default.
UPDATE github_check_runs g
SET completed = TRUE
FROM runs r
WHERE r.id = g.run_id
  AND r.status IN ('success', 'failed', 'canceled', 'skipped');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE github_check_runs DROP COLUMN completed;
-- +goose StatementEnd
