-- +goose Up
-- +goose StatementBegin

-- GitHub merge queue support (#261): a `merge_group.destroyed` delivery cancels
-- active runs for the abandoned merge-group SHA and asks the scheduler to fire
-- external effects (CancelJob frames + service cleanup). LISTEN/NOTIFY is not
-- durable, so mirror the supersede-effects claim/done markers on runs.
ALTER TABLE runs ADD COLUMN merge_group_cancel_effects_claimed_at TIMESTAMPTZ;
ALTER TABLE runs ADD COLUMN merge_group_cancel_effects_at         TIMESTAMPTZ;

-- Replay lookup for canceled merge-group runs whose external effects have not
-- completed. Partial index keeps the scheduler tick bounded by pending work.
CREATE INDEX runs_merge_group_cancel_effects_pending_idx ON runs (finished_at)
  WHERE cause = 'merge_group'
    AND status = 'canceled'
    AND merge_group_cancel_effects_at IS NULL;

-- Hot path for `merge_group.destroyed`: locate only live merge-group runs for
-- the abandoned head SHA. The expression rides cause_detail because the head SHA
-- is provider metadata, while revisions keeps the material keyed by id.
CREATE INDEX runs_merge_group_live_head_sha_idx
  ON runs ((cause_detail->>'mg_head_sha'))
  WHERE cause = 'merge_group' AND status IN ('queued', 'running');

-- `merge_group` is a system cancel origin: a destroyed queue ref should be
-- resurrectable by rerun like supersede/dependency, unlike a user's deliberate
-- single-job cancel.
ALTER TABLE job_runs DROP CONSTRAINT job_runs_cancel_origin_check;
ALTER TABLE job_runs
    ADD CONSTRAINT job_runs_cancel_origin_check
    CHECK (cancel_origin IS NULL OR cancel_origin IN
        ('user_job', 'user_run', 'supersede', 'approval_expiry', 'dependency', 'merge_group'))
    NOT VALID;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Preserve audit history on downgrade by mapping the new system origin to the
-- closest pre-#261 system origin before restoring the narrower CHECK.
UPDATE job_runs SET cancel_origin = 'dependency' WHERE cancel_origin = 'merge_group';
ALTER TABLE job_runs DROP CONSTRAINT job_runs_cancel_origin_check;
ALTER TABLE job_runs
    ADD CONSTRAINT job_runs_cancel_origin_check
    CHECK (cancel_origin IS NULL OR cancel_origin IN
        ('user_job', 'user_run', 'supersede', 'approval_expiry', 'dependency'));

DROP INDEX IF EXISTS runs_merge_group_live_head_sha_idx;
DROP INDEX IF EXISTS runs_merge_group_cancel_effects_pending_idx;
ALTER TABLE runs DROP COLUMN IF EXISTS merge_group_cancel_effects_at;
ALTER TABLE runs DROP COLUMN IF EXISTS merge_group_cancel_effects_claimed_at;

-- +goose StatementEnd
