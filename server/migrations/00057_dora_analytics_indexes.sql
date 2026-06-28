-- +goose NO TRANSACTION
-- +goose Up

-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_deployment_revisions_dora_window;
-- +goose StatementEnd

-- +goose StatementBegin

-- DORA analytics scans terminal deployment markers by environment and trailing
-- finished_at window. The existing deployment indexes serve "current version"
-- and per-environment history, but not the cross-project rollup path.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deployment_revisions_dora_window
    ON deployment_revisions (environment_id, finished_at)
    WHERE status IN ('success', 'failed')
      AND finished_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_deployment_revisions_dora_window;
-- +goose StatementEnd
