-- name: UpsertSecret :one
-- Upserts a (project_id, name) -> value_enc pair. updated_at always bumps on
-- update because the ciphertext changes (random nonce) even for identical
-- plaintext, making a "was this changed" diff unreliable; we bump
-- unconditionally on write.
INSERT INTO secrets (project_id, name, value_enc)
VALUES ($1, $2, $3)
ON CONFLICT (project_id, name) DO UPDATE SET
    value_enc  = EXCLUDED.value_enc,
    updated_at = NOW()
RETURNING id, project_id, name, created_at, updated_at, (xmax = 0) AS created;

-- name: ListSecretsByProject :many
-- Lists names + timestamps only — values never leave the DB without going
-- through GetSecretValuesByProject below.
SELECT name, created_at, updated_at
FROM secrets
WHERE project_id = $1
ORDER BY name;

-- name: GetSecretValuesByProject :many
-- Used by the scheduler when a job declares `secrets: [FOO, BAR]`. Returns
-- the encrypted blobs; the caller decrypts and injects as env vars.
SELECT name, value_enc
FROM secrets
WHERE project_id = $1 AND name = ANY($2::text[]);

-- name: DeleteSecretByName :execrows
DELETE FROM secrets WHERE project_id = $1 AND name = $2;
