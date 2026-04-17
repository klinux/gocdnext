-- name: UpsertProject :one
INSERT INTO projects (slug, name, description)
VALUES ($1, $2, $3)
ON CONFLICT (slug) DO UPDATE SET
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    updated_at = NOW()
RETURNING id, slug, name, description, created_at, updated_at, (xmax = 0) AS created;

-- name: FindProjectBySlug :one
SELECT id, slug, name, description, created_at, updated_at
FROM projects
WHERE slug = $1
LIMIT 1;
