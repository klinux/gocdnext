-- +goose Up
-- +goose StatementBegin
-- required_checks holds the per-project "required pipelines for PR merge" config
-- PLUS the last-sync state of the dedicated GitHub ruleset gocdnext manages for
-- it. NULL = the feature is not configured for the project. JSON shape:
--   {"pipelines":["build","e2e"],"ruleset_id":123,"synced_at":"...","sync_error":""}
-- `pipelines` are the ones whose commit-status check `ci/gocdnext/<slug>/<name>`
-- must be green before a PR can be merged; `ruleset_id` is the id of the
-- per-project `gocdnext-required-checks-<slug>` ruleset written to the repo
-- (upsert-in-place);
-- `synced_at`/`sync_error` record the outcome of the last reconcile so the UI can
-- surface drift or a missing-permission failure without a silent half-apply.
ALTER TABLE projects ADD COLUMN required_checks JSONB;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE projects DROP COLUMN required_checks;
-- +goose StatementEnd
