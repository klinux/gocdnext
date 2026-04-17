-- name: FindMaterialByFingerprint :one
SELECT id, pipeline_id, type, config, fingerprint, auto_update, created_at
FROM materials
WHERE fingerprint = $1
LIMIT 1;

-- name: InsertMaterial :one
INSERT INTO materials (pipeline_id, type, config, fingerprint, auto_update)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, pipeline_id, type, config, fingerprint, auto_update, created_at;

-- name: UpsertMaterial :one
INSERT INTO materials (pipeline_id, type, config, fingerprint, auto_update)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (pipeline_id, fingerprint) DO UPDATE SET
    type = EXCLUDED.type,
    config = EXCLUDED.config,
    auto_update = EXCLUDED.auto_update
RETURNING id, pipeline_id, type, config, fingerprint, auto_update, created_at, (xmax = 0) AS created;

-- name: ListMaterialsByPipeline :many
SELECT id, pipeline_id, type, config, fingerprint, auto_update, created_at
FROM materials
WHERE pipeline_id = $1
ORDER BY fingerprint;

-- name: DeleteMaterial :exec
DELETE FROM materials WHERE id = $1;
