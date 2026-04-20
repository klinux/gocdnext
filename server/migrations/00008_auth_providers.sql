-- +goose Up
-- +goose StatementBegin

-- DB-backed OIDC / OAuth provider config so admins can add + edit
-- identity providers from the /settings/auth UI without a server
-- restart. Env-var-provided providers still win if they share a
-- `name` (bootstrap path: a fresh deployment with env vars set
-- stays reachable even when the DB table is empty).
--
-- client_secret is stored encrypted with the same AES-GCM cipher
-- used for /secrets (GOCDNEXT_SECRET_KEY). A DB dump without the
-- key buys nothing.
CREATE TABLE auth_providers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL UNIQUE,
    kind            TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    client_id       TEXT NOT NULL,
    client_secret   BYTEA NOT NULL,
    issuer          TEXT NOT NULL DEFAULT '',
    github_api_base TEXT NOT NULL DEFAULT '',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT auth_providers_kind_check CHECK (kind IN ('github', 'oidc'))
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS auth_providers;
-- +goose StatementEnd
