-- name: FindMaterialsByFingerprint :many
-- N rows per fingerprint by design: materials are uniqued on
-- (pipeline_id, fingerprint), so several pipelines that watch the
-- same (repo, branch) legitimately share a hash. ORDER BY pipeline_id
-- makes the dispatch order deterministic across replays / reaper
-- requeues / multi-replica races. An earlier :one LIMIT 1 form
-- silently kept only the first row, so only ONE pipeline fan-out
-- happened on every push and "which one" was non-deterministic.
SELECT id, pipeline_id, type, config, fingerprint, auto_update, created_at
FROM materials
WHERE fingerprint = $1
ORDER BY pipeline_id;

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
