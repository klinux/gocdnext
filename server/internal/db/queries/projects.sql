-- name: UpsertProject :one
-- config_path defaults to '.gocdnext' at the SQL level. Apply
-- callers that don't set it (legacy path, drift re-apply) get
-- that default naturally via COALESCE on the column-side default.
-- Callers that DO set it override via ON CONFLICT.
INSERT INTO projects (slug, name, description, config_path)
VALUES ($1, $2, $3, COALESCE(NULLIF(@config_path::text, ''), '.gocdnext'))
ON CONFLICT (slug) DO UPDATE SET
    name        = EXCLUDED.name,
    description = EXCLUDED.description,
    config_path = COALESCE(NULLIF(EXCLUDED.config_path, ''), projects.config_path),
    updated_at  = NOW()
RETURNING id, slug, name, description, config_path, created_at, updated_at, (xmax = 0) AS created;

-- name: FindProjectBySlug :one
SELECT id, slug, name, description, config_path, created_at, updated_at
FROM projects
WHERE slug = $1
LIMIT 1;

-- name: GetProjectDeletionCounts :one
-- Aggregated before the cascading delete so the caller can surface
-- "deleted N pipelines, M runs, K secrets" without probing each
-- table after the fact (by then the rows are gone). Kept as a
-- single round-trip so the delete flow stays two calls, not six.
SELECT
    (SELECT COUNT(*) FROM pipelines WHERE project_id = p.id)::bigint        AS pipeline_count,
    (SELECT COUNT(*) FROM runs r
        JOIN pipelines pl ON pl.id = r.pipeline_id
        WHERE pl.project_id = p.id)::bigint                                  AS run_count,
    (SELECT COUNT(*) FROM secrets WHERE project_id = p.id)::bigint           AS secret_count,
    (SELECT COUNT(*) FROM scm_sources WHERE project_id = p.id)::bigint       AS scm_source_count
FROM projects p
WHERE p.slug = $1;

-- name: DeleteProjectBySlug :execrows
-- Returns the number of project rows deleted (0 or 1). ON DELETE
-- CASCADE on every foreign key that points at projects carries
-- the children (pipelines → materials → runs → artifacts, secrets,
-- scm_sources, etc.), so this single statement is enough.
DELETE FROM projects WHERE slug = $1;

-- name: GetProjectNotifications :one
-- Returns the project-level notifications JSONB array. The run-
-- create path consults this when the pipeline's own `notifications:`
-- block is absent (pipeline nil means "inherit"; pipeline empty
-- list means "explicit opt-out" and we skip this entirely).
SELECT notifications
FROM projects
WHERE id = $1;

-- name: SetProjectNotifications :exec
-- Replaces the project-level notifications list. Admin/maintainer
-- UI writes here; the column has a NOT NULL default of '[]' so a
-- fresh project never needs an initial INSERT against this field.
UPDATE projects
SET notifications = $2, updated_at = NOW()
WHERE id = $1;
