-- +goose Up
-- +goose StatementBegin
-- first_commit_at = the earliest commit on the PR (fetched from the provider
-- API when the PR opens). The Coding stage of DORA lead time is
-- first_commit_at → opened_at.
ALTER TABLE vcs_pull_requests ADD COLUMN first_commit_at TIMESTAMPTZ;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE vcs_pull_requests DROP COLUMN IF EXISTS first_commit_at;
-- +goose StatementEnd
