-- +goose Up
-- +goose StatementBegin

-- Daily materialized rollup of terminal deploy outcomes per environment — phase
-- 1b of the analytics-scale epic (#128), the deploy/DORA mirror of
-- analytics_run_daily. Counts only (additive across days); lead time + MTTR stay
-- live (percentiles can't be summed across buckets). deploys_failed folds
-- status='failed' OR is_rollback (a rollback is a change-failure signal, even if
-- the rollback deploy itself "succeeded") — same semantics as the live DORA
-- queries. Bucketed by finished_at::date. Refreshed by the analytics rollup
-- refresher; an environment delete cascades.
CREATE TABLE analytics_deploy_daily (
    environment_id  UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    day             DATE NOT NULL,
    deploys_success BIGINT NOT NULL DEFAULT 0,
    deploys_total   BIGINT NOT NULL DEFAULT 0,
    deploys_failed  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (environment_id, day)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_analytics_deploy_daily_day ON analytics_deploy_daily (day);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS analytics_deploy_daily;
-- +goose StatementEnd
