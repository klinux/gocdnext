-- name: GetPlatformSetting :one
-- Single-row lookup by string key. Returns the full envelope so
-- the caller can decrypt the secret half locally with the cipher
-- (no plaintext crosses the store boundary unless the caller
-- explicitly decrypts).
SELECT key, value, credentials_enc, updated_at, updated_by
FROM platform_settings
WHERE key = $1
LIMIT 1;

-- name: UpsertPlatformSetting :one
-- Idempotent write. Replaces value + credentials_enc on key
-- collision. updated_at refreshes on every write so audit / UI
-- "last changed" stays honest.
INSERT INTO platform_settings (key, value, credentials_enc, updated_at, updated_by)
VALUES ($1, $2, $3, NOW(), $4)
ON CONFLICT (key) DO UPDATE
    SET value           = EXCLUDED.value,
        credentials_enc = EXCLUDED.credentials_enc,
        updated_at      = NOW(),
        updated_by      = EXCLUDED.updated_by
RETURNING key, value, credentials_enc, updated_at, updated_by;

-- name: DeletePlatformSetting :exec
-- Removes a setting; the boot path then falls back to env config.
DELETE FROM platform_settings WHERE key = $1;
