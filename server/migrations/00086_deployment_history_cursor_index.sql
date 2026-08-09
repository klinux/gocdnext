-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deployment_revisions_history_cursor
    ON deployment_revisions (environment_id, created_at DESC, id DESC);

DROP INDEX CONCURRENTLY IF EXISTS idx_deployment_revisions_history;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deployment_revisions_history
    ON deployment_revisions (environment_id, created_at DESC);

DROP INDEX CONCURRENTLY IF EXISTS idx_deployment_revisions_history_cursor;
