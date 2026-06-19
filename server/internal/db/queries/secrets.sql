-- name: UpsertSecret :one
-- Upserts a (project_id, name) entry. A db-source secret carries value_enc;
-- an external reference carries source + ref_path[/ref_key] and NULL value.
-- updated_at always bumps on update (ciphertext nonce changes even for an
-- identical plaintext, so a "was this changed" diff is unreliable).
-- Targets the partial UNIQUE index secrets_project_name_idx.
INSERT INTO secrets (project_id, name, value_enc, source, ref_path, ref_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (project_id, name) WHERE project_id IS NOT NULL DO UPDATE SET
    value_enc  = EXCLUDED.value_enc,
    source     = EXCLUDED.source,
    ref_path   = EXCLUDED.ref_path,
    ref_key    = EXCLUDED.ref_key,
    updated_at = NOW()
RETURNING id, project_id, name, created_at, updated_at, (xmax = 0) AS created;

-- name: UpsertGlobalSecret :one
-- Global scope: project_id = NULL, shadowed by a same-name project secret
-- at resolution time. Targets the partial UNIQUE index
-- secrets_global_name_idx (predicate spelled out so Postgres picks it).
INSERT INTO secrets (project_id, name, value_enc, source, ref_path, ref_key)
VALUES (NULL, $1, $2, $3, $4, $5)
ON CONFLICT (name) WHERE project_id IS NULL DO UPDATE SET
    value_enc  = EXCLUDED.value_enc,
    source     = EXCLUDED.source,
    ref_path   = EXCLUDED.ref_path,
    ref_key    = EXCLUDED.ref_key,
    updated_at = NOW()
RETURNING id, name, created_at, updated_at, (xmax = 0) AS created;

-- name: ListSecretsByProject :many
-- Names + source/ref + timestamps only — values never leave the DB except
-- via the resolver. Used for the unpaginated inherited-globals panel.
SELECT name, source, ref_path, ref_key, created_at, updated_at
FROM secrets
WHERE project_id = $1
ORDER BY name;

-- name: ListGlobalSecrets :many
-- Admin-only listing of every global (unscoped) secret. Same
-- "names + source/ref, never values" contract.
SELECT name, source, ref_path, ref_key, created_at, updated_at
FROM secrets
WHERE project_id IS NULL
ORDER BY name;

-- name: ListSecretsByProjectPaged :many
-- Paginated project listing (admin/project secrets page). ORDER BY name so
-- offset paging is stable. Companion count in CountSecretsByProject.
SELECT name, source, ref_path, ref_key, created_at, updated_at
FROM secrets
WHERE project_id = $1
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: CountSecretsByProject :one
SELECT count(*) FROM secrets WHERE project_id = $1;

-- name: ListGlobalSecretsPaged :many
SELECT name, source, ref_path, ref_key, created_at, updated_at
FROM secrets
WHERE project_id IS NULL
ORDER BY name
LIMIT $1 OFFSET $2;

-- name: CountGlobalSecrets :one
SELECT count(*) FROM secrets WHERE project_id IS NULL;

-- name: GetSecretEntriesByProject :many
-- Dispatch path: a job declared `secrets: [FOO, BAR]`. Returns the full
-- entry (value_enc for db rows, source+ref for external rows); the
-- CompositeResolver decrypts or fetches per source.
SELECT name, value_enc, source, ref_path, ref_key
FROM secrets
WHERE project_id = $1 AND name = ANY($2::text[]);

-- name: GetGlobalSecretEntries :many
-- Resolver fallback for names still missing at project scope.
SELECT name, value_enc, source, ref_path, ref_key
FROM secrets
WHERE project_id IS NULL AND name = ANY($1::text[]);

-- name: DeleteSecretByName :execrows
DELETE FROM secrets WHERE project_id = $1 AND name = $2;

-- name: DeleteGlobalSecretByName :execrows
DELETE FROM secrets WHERE project_id IS NULL AND name = $1;
