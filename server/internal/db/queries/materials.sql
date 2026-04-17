-- name: FindMaterialByFingerprint :one
SELECT id, pipeline_id, type, config, fingerprint, auto_update, created_at
FROM materials
WHERE fingerprint = $1
LIMIT 1;

-- name: InsertMaterial :one
INSERT INTO materials (pipeline_id, type, config, fingerprint, auto_update)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, pipeline_id, type, config, fingerprint, auto_update, created_at;
