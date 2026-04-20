-- +goose Up
-- +goose StatementBegin

-- Per-project config folder. `.gocdnext/` is still the default
-- — every existing row flips to it — but teams with a different
-- convention (`.woodpecker/`, `.ci/`, monorepo paths like
-- `apps/foo/.gocdnext/`) can override via the projects dialog or
-- the apply endpoint. Hardcoded constants in parser + github
-- contents fetcher go away in the same slice.
ALTER TABLE projects
    ADD COLUMN config_path TEXT NOT NULL DEFAULT '.gocdnext';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE projects DROP COLUMN IF EXISTS config_path;
-- +goose StatementEnd
