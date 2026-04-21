-- name: UpsertSecret :one
-- Upserts a (project_id, name) -> value_enc pair. updated_at always bumps on
-- update because the ciphertext changes (random nonce) even for identical
-- plaintext, making a "was this changed" diff unreliable; we bump
-- unconditionally on write.
--
-- Targets the partial UNIQUE index secrets_project_name_idx (rows
-- where project_id IS NOT NULL). The global twin lives in
-- UpsertGlobalSecret below.
INSERT INTO secrets (project_id, name, value_enc)
VALUES ($1, $2, $3)
ON CONFLICT (project_id, name) WHERE project_id IS NOT NULL DO UPDATE SET
    value_enc  = EXCLUDED.value_enc,
    updated_at = NOW()
RETURNING id, project_id, name, created_at, updated_at, (xmax = 0) AS created;

-- name: UpsertGlobalSecret :one
-- Global scope: project_id = NULL, shadowed by a same-name project
-- secret at resolution time. Targets the partial UNIQUE index
-- secrets_global_name_idx — Postgres can't infer partial indexes
-- from ON CONFLICT (name) alone, so we spell out the predicate.
INSERT INTO secrets (project_id, name, value_enc)
VALUES (NULL, $1, $2)
ON CONFLICT (name) WHERE project_id IS NULL DO UPDATE SET
    value_enc  = EXCLUDED.value_enc,
    updated_at = NOW()
RETURNING id, name, created_at, updated_at, (xmax = 0) AS created;

-- name: ListSecretsByProject :many
-- Lists names + timestamps only — values never leave the DB without going
-- through GetSecretValuesByProject below.
SELECT name, created_at, updated_at
FROM secrets
WHERE project_id = $1
ORDER BY name;

-- name: ListGlobalSecrets :many
-- Admin-only listing of every global (unscoped) secret. Same
-- "names + timestamps, never values" contract as project secrets.
SELECT name, created_at, updated_at
FROM secrets
WHERE project_id IS NULL
ORDER BY name;

-- name: GetSecretValuesByProject :many
-- Used by the scheduler when a job declares `secrets: [FOO, BAR]`. Returns
-- the encrypted blobs; the caller decrypts and injects as env vars.
SELECT name, value_enc
FROM secrets
WHERE project_id = $1 AND name = ANY($2::text[]);

-- name: GetGlobalSecretValues :many
-- Resolver fallback: after GetSecretValuesByProject, names still
-- missing are looked up as globals. Kept as a separate call so
-- the resolver can short-circuit when every name was already
-- covered at project scope.
SELECT name, value_enc
FROM secrets
WHERE project_id IS NULL AND name = ANY($1::text[]);

-- name: DeleteSecretByName :execrows
DELETE FROM secrets WHERE project_id = $1 AND name = $2;

-- name: DeleteGlobalSecretByName :execrows
DELETE FROM secrets WHERE project_id IS NULL AND name = $1;
