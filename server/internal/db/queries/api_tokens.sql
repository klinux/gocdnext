-- name: InsertAPIToken :one
-- Stores a freshly-minted token. Caller passes the SHA-256 hex
-- digest in `hash`; the plaintext is shown to the user once at
-- creation time and never persisted. Either user_id OR
-- service_account_id is set (XOR enforced by the table check).
INSERT INTO api_tokens (
    user_id, service_account_id, name, hash, prefix, expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, service_account_id, name, prefix,
          expires_at, last_used_at, revoked_at, created_at;

-- name: GetAPITokenByHash :one
-- Hot path: the bearer middleware probes this on every request.
-- Returns NOT FOUND when revoked or expired so the middleware
-- doesn't have to special-case those.
SELECT id, user_id, service_account_id, name, prefix,
       expires_at, last_used_at, revoked_at, created_at
FROM api_tokens
WHERE hash = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > NOW());

-- name: TouchAPITokenLastUsed :exec
-- Best-effort `last_used_at` bump. Called from the middleware
-- when a Bearer token authenticates successfully — a stale value
-- doesn't break anything, just makes the audit trail less useful.
UPDATE api_tokens
SET last_used_at = NOW()
WHERE id = $1;

-- name: ListAPITokensByUser :many
-- Tokens this user owns, newest first. Used by /settings/api-tokens.
SELECT id, name, prefix, expires_at, last_used_at, revoked_at, created_at
FROM api_tokens
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: ListAPITokensByServiceAccount :many
SELECT id, name, prefix, expires_at, last_used_at, revoked_at, created_at
FROM api_tokens
WHERE service_account_id = $1
ORDER BY created_at DESC;

-- name: RevokeAPIToken :exec
-- Idempotent: revoke-on-already-revoked is a no-op. Caller filters
-- by user_id or service_account_id to gate ownership.
UPDATE api_tokens
SET revoked_at = NOW()
WHERE id = $1 AND revoked_at IS NULL;

-- name: GetAPITokenForUserOwner :one
-- Used by handlers that need to verify "this token belongs to this
-- user before letting them revoke it" — not just any token.
SELECT id, name, prefix, expires_at, last_used_at, revoked_at
FROM api_tokens
WHERE id = $1 AND user_id = $2;

-- name: GetAPITokenForSAOwner :one
-- Same, scoped to a service account (admin path).
SELECT id, name, prefix, expires_at, last_used_at, revoked_at
FROM api_tokens
WHERE id = $1 AND service_account_id = $2;
