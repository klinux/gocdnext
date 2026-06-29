-- +goose Up
-- +goose StatementBegin

-- Daily materialized rollup of terminal run outcomes per pipeline — phase 1 of
-- the analytics-scale epic (#128). Counts only (additive across days); the
-- duration/queue percentiles stay live (a median can't be summed across
-- buckets). runs_failed folds 'failed' + 'errored'; 'canceled' is excluded (not
-- a success or a failure). Bucketed by finished_at::date. Refreshed
-- incrementally by the analytics rollup refresher; a pipeline delete cascades.
CREATE TABLE analytics_run_daily (
    pipeline_id  UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    day          DATE NOT NULL,
    runs_success BIGINT NOT NULL DEFAULT 0,
    runs_failed  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (pipeline_id, day)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- The cross-project rollup scans by trailing day window across pipelines.
CREATE INDEX idx_analytics_run_daily_day ON analytics_run_daily (day);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS analytics_run_daily;
-- +goose StatementEnd
