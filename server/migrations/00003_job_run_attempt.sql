-- +goose Up
-- +goose StatementBegin

-- Retry counter for the reaper: if an agent vanishes mid-job, the reaper
-- bumps attempt and re-queues. Once attempt hits the cap, the job fails so
-- the run can progress. Keeping it on the same row (instead of a separate
-- job_run_attempts table) is a deliberate MVP simplification: we lose per-
-- attempt log separation but the schema stays small.
ALTER TABLE job_runs ADD COLUMN attempt INT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_runs DROP COLUMN IF EXISTS attempt;
-- +goose StatementEnd
