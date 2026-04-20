-- name: ListAuthProviders :many
-- Returns every row regardless of `enabled`. The UI filters on
-- the boolean so admins can see disabled providers they can re-
-- enable later.
SELECT id, name, kind, display_name, client_id, client_secret,
       issuer, github_api_base, enabled, created_at, updated_at
FROM auth_providers
ORDER BY name ASC;

-- name: ListEnabledAuthProviders :many
-- Bootstrap path: just the ones that should be registered on
-- server start (or after a reload).
SELECT id, name, kind, display_name, client_id, client_secret,
       issuer, github_api_base, enabled, created_at, updated_at
FROM auth_providers
WHERE enabled = TRUE
ORDER BY name ASC;

-- name: GetAuthProviderByID :one
SELECT id, name, kind, display_name, client_id, client_secret,
       issuer, github_api_base, enabled, created_at, updated_at
FROM auth_providers
WHERE id = $1;

-- name: UpsertAuthProvider :one
-- ON CONFLICT (name) DO UPDATE bumps kind + display + id/secret
-- + issuer + api base + enabled. We never update created_at.
INSERT INTO auth_providers (
    name, kind, display_name, client_id, client_secret,
    issuer, github_api_base, enabled
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (name) DO UPDATE SET
    kind            = EXCLUDED.kind,
    display_name    = EXCLUDED.display_name,
    client_id       = EXCLUDED.client_id,
    client_secret   = EXCLUDED.client_secret,
    issuer          = EXCLUDED.issuer,
    github_api_base = EXCLUDED.github_api_base,
    enabled         = EXCLUDED.enabled,
    updated_at      = NOW()
RETURNING id, name, kind, display_name, client_id, client_secret,
          issuer, github_api_base, enabled, created_at, updated_at;

-- name: DeleteAuthProvider :exec
DELETE FROM auth_providers WHERE id = $1;

-- name: SetAuthProviderEnabled :exec
UPDATE auth_providers
SET enabled = $2, updated_at = NOW()
WHERE id = $1;
