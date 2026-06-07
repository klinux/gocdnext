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

-- name: ListGitMaterials :many
-- Every type='git' material in the DB. Webhook tag-push routing
-- pulls this set and filters Go-side: NormalizeGitURL on each
-- config->>'url' matched against the canonical clone URL from the
-- webhook payload. URL drift across the operator-typed forms
-- (`.git` suffix, https vs ssh) is canonicalised on BOTH sides at
-- filter time so a material declared with `url:
-- https://github.com/x/y.git` matches a webhook for
-- `https://github.com/x/y` and vice versa.
--
-- Why a full scan vs an indexed `WHERE config->>'url' = $1`:
-- material URLs are stored verbatim from the YAML (parser keeps the
-- operator's form so the agent's `git clone` reproduces what was
-- declared), so the in-DB string varies across rows. A JSONB
-- functional index would only help for the exact form; the
-- normalize-and-compare path covers all of them. At < 10k git
-- materials the scan is < 100ms; revisit with an `url_fingerprint`
-- column if larger deployments observe latency.
SELECT id, pipeline_id, type, config, fingerprint, auto_update, created_at
FROM materials
WHERE type = 'git'
ORDER BY pipeline_id;

-- name: ListMaterialsByPipeline :many
SELECT id, pipeline_id, type, config, fingerprint, auto_update, created_at
FROM materials
WHERE pipeline_id = $1
ORDER BY fingerprint;

-- name: DeleteMaterial :exec
DELETE FROM materials WHERE id = $1;
