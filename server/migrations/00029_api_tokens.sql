-- +goose Up
-- +goose StatementBegin

-- Service accounts: machine identities for automation that don't
-- belong to any one user. Independent role; survive when the
-- creator leaves. Each SA can hold N tokens for rotation without
-- downtime — issue the new one, swap clients, revoke the old one.
CREATE TABLE service_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL CHECK (role IN ('admin', 'maintainer', 'viewer')),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- API tokens: belong to either a user OR a service account.
-- The XOR check enforces that exactly one of the two FKs is set —
-- a single tokens table keeps the lookup path short on the hot
-- middleware code, and the SA-vs-user distinction is one column
-- away whichever side hits.
--
-- `hash` is sha256 of the raw token body (post-prefix); we never
-- store the plaintext. `prefix` is the first 8 chars of the body
-- in the clear so the audit trail can identify which token was
-- used without exposing it ("ABC1...XY3 by alice@example.com").
CREATE TABLE api_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    service_account_id UUID REFERENCES service_accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    hash TEXT NOT NULL,
    prefix TEXT NOT NULL,
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT api_tokens_subject_xor CHECK (
        (user_id IS NOT NULL AND service_account_id IS NULL) OR
        (user_id IS NULL AND service_account_id IS NOT NULL)
    )
);

-- Hash uniqueness gates collision detection at insert time and
-- makes the validate-bearer path a single index probe.
CREATE UNIQUE INDEX api_tokens_hash_idx ON api_tokens(hash);
CREATE INDEX api_tokens_user_idx ON api_tokens(user_id) WHERE user_id IS NOT NULL;
CREATE INDEX api_tokens_sa_idx ON api_tokens(service_account_id) WHERE service_account_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS service_accounts;

-- +goose StatementEnd
