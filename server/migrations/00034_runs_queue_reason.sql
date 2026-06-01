-- +goose Up
-- +goose StatementBegin

-- runs.queue_reason carries an operator-visible explanation for runs
-- that landed in 'queued' status and didn't proceed to 'running' on
-- the next scheduler tick. Today the only producer is the serial-
-- concurrency gate (issue #4 path #2): when scheduler.dispatchRun
-- hits the gate via OtherRunningRunExistsForPipeline, it stamps
-- 'serial-busy:<predecessor-run-id>' here so the runs list / run
-- detail can render "waiting on #N" instead of a status-only badge.
--
-- Without this, the operator pushes a commit, the runs tab shows
-- a queued entry, and there's no signal whether the scheduler is
-- ticking, an agent is missing, or a predecessor run is hogging the
-- pipeline — every "stuck queued" looks identical.
--
-- TEXT instead of an enum so we can land Cut B without prescribing
-- the full taxonomy up front. Future producers (no-eligible-agent,
-- approval-pending, frozen-deploy, etc.) extend the vocabulary
-- without another migration. Format is a `key:detail` pair, both
-- pieces parseable client-side.
--
-- Cleared on transition to 'running' (scheduler.dispatchRun on the
-- happy path) and on terminal transitions (CompleteJobRun cascade)
-- so the field never linger past the queued window it describes.
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS queue_reason TEXT;

COMMENT ON COLUMN runs.queue_reason IS
    'Operator-visible reason a queued run is not advancing. Format key:detail. Cleared when the run leaves queued.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs
    DROP COLUMN IF EXISTS queue_reason;
-- +goose StatementEnd
