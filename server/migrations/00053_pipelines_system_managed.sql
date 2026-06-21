-- +goose Up
-- +goose StatementBegin
-- system_managed marks a pipeline the server owns and synthesises (today: the
-- `_compliance` enforcement pipeline created for a governed project with no
-- pipeline of its own). It is the EXPLICIT marker the governance reconciler uses
-- to decide what it may create/refresh/delete — so a pre-existing user pipeline
-- that happens to be named `_compliance` (system_managed defaults to FALSE) is
-- treated as a normal repo pipeline and never silently deleted.
ALTER TABLE pipelines ADD COLUMN system_managed BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE pipelines DROP COLUMN system_managed;
-- +goose StatementEnd
