-- +goose Up
-- +goose StatementBegin

-- Approval gates: job_runs with `approval_gate=true` never
-- dispatch; they park in status='awaiting_approval' until a
-- human clicks approve/reject. The run-creation path flips the
-- bit during stage/job materialisation based on the pipeline's
-- domain.Job.Approval. Everything lives on job_runs directly
-- (1:1 relationship) to keep dispatch queries single-table.

ALTER TABLE job_runs
    ADD COLUMN approval_gate        BOOLEAN     NOT NULL DEFAULT false,
    ADD COLUMN approvers            TEXT[]      NOT NULL DEFAULT '{}',
    ADD COLUMN approval_description TEXT,
    ADD COLUMN awaiting_since       TIMESTAMPTZ,
    ADD COLUMN decided_by           TEXT,
    ADD COLUMN decided_at           TIMESTAMPTZ,
    ADD COLUMN decision             TEXT;

-- Partial index — the "pending approvals" widget and any
-- project-scoped filter key on exactly this predicate. A full
-- index would bloat the existing queued/running index without
-- adding selectivity.
CREATE INDEX idx_job_runs_awaiting_approval
    ON job_runs (awaiting_since ASC)
    WHERE status = 'awaiting_approval';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_job_runs_awaiting_approval;
ALTER TABLE job_runs
    DROP COLUMN approval_gate,
    DROP COLUMN approvers,
    DROP COLUMN approval_description,
    DROP COLUMN awaiting_since,
    DROP COLUMN decided_by,
    DROP COLUMN decided_at,
    DROP COLUMN decision;
-- +goose StatementEnd
