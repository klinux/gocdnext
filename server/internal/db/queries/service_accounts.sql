-- name: InsertServiceAccount :one
INSERT INTO service_accounts (name, description, role, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id, name, description, role, created_by, disabled_at,
          created_at, updated_at;

-- name: GetServiceAccountByID :one
SELECT id, name, description, role, created_by, disabled_at,
       created_at, updated_at
FROM service_accounts
WHERE id = $1;

-- name: ListServiceAccounts :many
-- Newest first. Disabled SAs included; the UI dims them so admins
-- see the full picture without filtering.
SELECT id, name, description, role, created_by, disabled_at,
       created_at, updated_at
FROM service_accounts
ORDER BY created_at DESC;

-- name: UpdateServiceAccount :exec
UPDATE service_accounts
SET description = $2, role = $3, updated_at = NOW()
WHERE id = $1;

-- name: SetServiceAccountDisabled :exec
-- Disabling stops new tokens from authenticating (the bearer
-- middleware loads the SA after token lookup and bounces 401 when
-- disabled_at is set). Existing tokens stay in the table — wipe
-- explicitly with DELETE if you want them gone.
UPDATE service_accounts
SET disabled_at = $2, updated_at = NOW()
WHERE id = $1;

-- name: DeleteServiceAccount :exec
-- ON DELETE CASCADE on api_tokens cleans up the token rows
-- automatically.
DELETE FROM service_accounts WHERE id = $1;
