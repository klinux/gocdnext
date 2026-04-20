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
