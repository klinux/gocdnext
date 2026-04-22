-- +goose Up
-- +goose StatementBegin

-- user_preferences holds arbitrary per-user UI state (hidden
-- projects, default view mode, dashboard widget order, …) as a
-- single JSONB blob. JSONB over dedicated columns so the web app
-- can add a preference key without a new migration — most of
-- these settings don't need to be queryable from SQL, only
-- read/write as a whole document.
--
-- Known keys (kept in sync with the TypeScript schema):
--   hidden_projects  string[]  project UUIDs the user has hidden
--                              from the projects list
--
-- Future:
--   default_view     "grid" | "list"
--   hidden_groups    string[]  (once the groups feature ships)
CREATE TABLE user_preferences (
    user_id     UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    preferences JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS user_preferences;
-- +goose StatementEnd
