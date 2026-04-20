-- name: UpsertUserByProvider :one
-- Either inserts a fresh row (new login) or bumps the profile
-- fields we pull from the IdP every time. Role is intentionally
-- NOT overwritten on conflict — it's admin-assigned and must not
-- revert to 'user' just because the IdP doesn't carry it.
INSERT INTO users (email, name, avatar_url, provider, external_id, role, last_login_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (provider, external_id) DO UPDATE SET
    email        = EXCLUDED.email,
    name         = EXCLUDED.name,
    avatar_url   = EXCLUDED.avatar_url,
    last_login_at = NOW(),
    updated_at   = NOW()
RETURNING id, email, name, avatar_url, provider, external_id, role,
          disabled_at, last_login_at, created_at, updated_at;

-- name: GetUserByID :one
SELECT id, email, name, avatar_url, provider, external_id, role,
       disabled_at, last_login_at, created_at, updated_at
FROM users
WHERE id = $1;

-- name: ListUsers :many
SELECT id, email, name, avatar_url, provider, external_id, role,
       disabled_at, last_login_at, created_at, updated_at
FROM users
ORDER BY email ASC;

-- name: InsertAuthState :exec
INSERT INTO auth_states (state_hash, provider, redirect_to, nonce, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ConsumeAuthState :one
-- Single-use: delete as we read. Returning nothing = no such state
-- (or it expired and the sweeper got to it first).
DELETE FROM auth_states
WHERE state_hash = $1 AND expires_at > NOW()
RETURNING provider, redirect_to, nonce;

-- name: DeleteExpiredAuthStates :exec
DELETE FROM auth_states WHERE expires_at <= NOW();

-- name: InsertUserSession :exec
INSERT INTO user_sessions (id, user_id, expires_at, user_agent)
VALUES ($1, $2, $3, $4);

-- name: GetUserSession :one
-- Returns the session + its user row. Expired rows are filtered in
-- the query so a single round-trip tells the handler "yes/no".
SELECT u.id, u.email, u.name, u.avatar_url, u.provider, u.external_id,
       u.role, u.disabled_at, u.last_login_at, u.created_at, u.updated_at,
       s.expires_at, s.last_seen_at
FROM user_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = $1 AND s.expires_at > NOW();

-- name: TouchUserSession :exec
-- Cheap idempotent update; called at most once per request via a
-- debounce in the middleware so we don't rewrite the row on every
-- 2s dashboard poll.
UPDATE user_sessions
SET last_seen_at = NOW()
WHERE id = $1;

-- name: DeleteUserSession :exec
DELETE FROM user_sessions WHERE id = $1;

-- name: DeleteUserSessionsForUser :exec
DELETE FROM user_sessions WHERE user_id = $1;

-- name: DeleteExpiredUserSessions :exec
DELETE FROM user_sessions WHERE expires_at <= NOW();
