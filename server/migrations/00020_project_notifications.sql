-- +goose Up
-- +goose StatementBegin

-- Project-level notifications: the same `notifications:` shape a
-- pipeline accepts, but scoped to the whole project. Pipelines
-- that don't declare their own `notifications:` block inherit
-- this list at run-create time; pipelines that DO declare one
-- (even an empty list) override project-level entirely. Keeping
-- this in a column on `projects` rather than a sibling table
-- keeps the write path to a single UPDATE and the read path to
-- a single SELECT, at the cost of a small amount of denormal-
-- isation. The default '[]' means "no project-level entries"
-- so existing projects + their runs behave exactly as before
-- (backwards compatible on deploy).

ALTER TABLE projects
    ADD COLUMN notifications JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE projects
    DROP COLUMN IF EXISTS notifications;
-- +goose StatementEnd
