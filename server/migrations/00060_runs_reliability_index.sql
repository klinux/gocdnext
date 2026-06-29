-- +goose NO TRANSACTION
-- +goose Up

-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_reliability_window;
-- +goose StatementEnd

-- +goose StatementBegin

-- The throughput/reliability analytics (#107 phase 3) scan terminal runs by
-- pipeline over a trailing finished_at window. The existing run indexes serve
-- per-pipeline counter order and the queued/running scheduler probe, but not
-- this terminal-history rollup. Run-based mirror of the DORA deployment index
-- (00057). Partial so it stays small (only terminal, finished rows).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_runs_reliability_window
    ON runs (pipeline_id, finished_at)
    WHERE status IN ('success', 'failed', 'errored')
      AND finished_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_runs_reliability_window;
-- +goose StatementEnd
