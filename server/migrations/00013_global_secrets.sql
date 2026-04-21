-- +goose Up
-- +goose StatementBegin

-- Make project_id nullable so a NULL row represents a global
-- secret — available to every pipeline at resolution time unless
-- a project-scoped row with the same name shadows it. Keeping
-- both scopes in one table means the resolver's "lookup by name"
-- is a single query instead of two, and rotation/audit tooling
-- written for project secrets extends to globals for free.
ALTER TABLE secrets
    ALTER COLUMN project_id DROP NOT NULL;

-- The previous UNIQUE(project_id, name) allowed (NULL, 'X') and
-- (NULL, 'X') to coexist because NULL != NULL in a non-distinct
-- comparison. Drop it and replace with two partial indexes so:
--   * project rows: one (project_id, name) per project — same as before
--   * global rows: one row per name globally
-- Both indexes are UNIQUE so UpsertSecret's ON CONFLICT clauses
-- can target the right one via partial-index inference.
ALTER TABLE secrets
    DROP CONSTRAINT IF EXISTS secrets_project_id_name_key;

CREATE UNIQUE INDEX secrets_project_name_idx
    ON secrets (project_id, name)
    WHERE project_id IS NOT NULL;

CREATE UNIQUE INDEX secrets_global_name_idx
    ON secrets (name)
    WHERE project_id IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Rolling back requires there to be no global rows; nuke them
-- rather than crashing the migration mid-way.
DELETE FROM secrets WHERE project_id IS NULL;

DROP INDEX IF EXISTS secrets_global_name_idx;
DROP INDEX IF EXISTS secrets_project_name_idx;

ALTER TABLE secrets
    ADD CONSTRAINT secrets_project_id_name_key UNIQUE (project_id, name);

ALTER TABLE secrets
    ALTER COLUMN project_id SET NOT NULL;
-- +goose StatementEnd
