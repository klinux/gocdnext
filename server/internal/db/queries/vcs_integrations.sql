-- name: ListVCSIntegrations :many
-- Admin feed. Returns every row regardless of `enabled` so the
-- /settings/integrations page can surface disabled rows the user
-- can re-enable.
SELECT id, kind, name, display_name, app_id, private_key,
       webhook_secret, api_base, enabled, created_at, updated_at
FROM vcs_integrations
ORDER BY name ASC;

-- name: ListEnabledVCSIntegrations :many
-- Bootstrap + reload path: rows the registry should actually
-- instantiate on startup.
SELECT id, kind, name, display_name, app_id, private_key,
       webhook_secret, api_base, enabled, created_at, updated_at
FROM vcs_integrations
WHERE enabled = TRUE
ORDER BY name ASC;

-- name: GetVCSIntegrationByID :one
SELECT id, kind, name, display_name, app_id, private_key,
       webhook_secret, api_base, enabled, created_at, updated_at
FROM vcs_integrations
WHERE id = $1;

-- name: UpsertVCSIntegration :one
-- ON CONFLICT (name) DO UPDATE refreshes every field EXCEPT
-- private_key + webhook_secret when the caller passes NULL.
-- The handler interprets an empty string from the dialog as
-- "keep existing ciphertext", mirroring auth_providers.
INSERT INTO vcs_integrations (
    kind, name, display_name, app_id,
    private_key, webhook_secret, api_base, enabled
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (name) DO UPDATE SET
    kind            = EXCLUDED.kind,
    display_name    = EXCLUDED.display_name,
    app_id          = EXCLUDED.app_id,
    private_key     = COALESCE(EXCLUDED.private_key, vcs_integrations.private_key),
    webhook_secret  = COALESCE(EXCLUDED.webhook_secret, vcs_integrations.webhook_secret),
    api_base        = EXCLUDED.api_base,
    enabled         = EXCLUDED.enabled,
    updated_at      = NOW()
RETURNING id, kind, name, display_name, app_id, private_key,
          webhook_secret, api_base, enabled, created_at, updated_at;

-- name: DeleteVCSIntegration :exec
DELETE FROM vcs_integrations WHERE id = $1;

-- name: SetVCSIntegrationEnabled :exec
UPDATE vcs_integrations
SET enabled = $2, updated_at = NOW()
WHERE id = $1;
