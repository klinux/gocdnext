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

-- name: UpdateUserRole :one
-- Flip a user's role. The CHECK constraint on the column enforces
-- the enum; an admin calling this with a typo'd value gets a clean
-- error from Postgres instead of a silent wrong-role write.
UPDATE users
SET role = $2, updated_at = NOW()
WHERE id = $1
RETURNING id, email, name, avatar_url, provider, external_id, role,
          disabled_at, last_login_at, created_at, updated_at;

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

-- name: CountLocalUsers :one
-- Drives the "show local login form" decision on the login page:
-- no rows = zero local admins, the form stays hidden so the page
-- doesn't advertise a dead code path.
SELECT COUNT(*)::bigint
FROM users
WHERE provider = 'local' AND password_hash IS NOT NULL;

-- name: GetLocalUserForLogin :one
-- Fetches the row we need to bcrypt-compare on a login POST.
-- Filtering by provider keeps an OIDC user with the same email
-- from accidentally answering to a password form.
SELECT id, email, name, avatar_url, provider, external_id, role,
       disabled_at, last_login_at, created_at, updated_at, password_hash
FROM users
WHERE provider = 'local' AND email = $1
LIMIT 1;

-- name: UpsertLocalUser :one
-- Create/update a local account. Called from the CLI and the
-- admin self-service change-password endpoint. external_id is
-- the email so the (provider, external_id) unique key covers us.
INSERT INTO users (email, name, avatar_url, provider, external_id, role, password_hash)
VALUES ($1, $2, '', 'local', $1, $3, $4)
ON CONFLICT (provider, external_id) DO UPDATE SET
    name          = EXCLUDED.name,
    role          = EXCLUDED.role,
    password_hash = EXCLUDED.password_hash,
    updated_at    = NOW()
RETURNING id, email, name, avatar_url, provider, external_id, role,
          disabled_at, last_login_at, created_at, updated_at;

-- name: UpdateLocalUserPassword :exec
-- Dedicated password-only write. Used when an admin changes their
-- own password from /settings/account — we never want to let the
-- admin also flip their role through that surface.
UPDATE users
SET password_hash = $2, updated_at = NOW()
WHERE id = $1 AND provider = 'local';
