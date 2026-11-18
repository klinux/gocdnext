-- +goose NO TRANSACTION
-- +goose Up

-- +goose StatementBegin
-- The build-time analytics anti-join (NOT EXISTS deployment_revisions WHERE
-- run_id = r.id) excludes deploy runs from the build-duration percentiles. Without
-- an index on run_id, Postgres scans the whole deployments table twice per
-- dashboard load. Partial so it stays small (run_id is nullable). Concurrent so
-- the migration doesn't lock deploy writes.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deployment_revisions_run_id
    ON deployment_revisions (run_id)
    WHERE run_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_deployment_revisions_run_id;
-- +goose StatementEnd
