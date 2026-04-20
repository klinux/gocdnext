-- +goose Up
-- +goose StatementBegin

-- Human operators of the control plane. One row per (provider,
-- external_id): a user who signs in through GitHub and then Google
-- creates two rows because we don't try to auto-merge identities.
-- Role is a free-text label with an enforced enum; adding new roles
-- later is a one-line migration, not a schema rework.
CREATE TABLE users (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email        TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    avatar_url   TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'user',
    disabled_at  TIMESTAMPTZ,
    last_login_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT users_role_check CHECK (role IN ('admin', 'user', 'viewer')),
    UNIQUE (provider, external_id)
);

CREATE INDEX idx_users_email ON users (email);

-- Session storage. `id` is a sha-256 hash of the opaque cookie
-- token; we never store the plaintext, so a DB leak can't be used
-- to impersonate. Expiry is enforced both in code (fast path) and
-- by the sweeper (garbage collection).
CREATE TABLE user_sessions (
    id           BYTEA PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    user_agent   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_user_sessions_user ON user_sessions (user_id);
CREATE INDEX idx_user_sessions_expires ON user_sessions (expires_at);

-- OAuth state parameter, hashed. Short-lived; we delete on consume.
-- Rows past expires_at are fair game for the sweeper too.
CREATE TABLE auth_states (
    state_hash   BYTEA PRIMARY KEY,
    provider     TEXT NOT NULL,
    redirect_to  TEXT NOT NULL DEFAULT '',
    nonce        TEXT NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_auth_states_expires ON auth_states (expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS auth_states;
DROP TABLE IF EXISTS user_sessions;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
