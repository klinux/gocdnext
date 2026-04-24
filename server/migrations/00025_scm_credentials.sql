-- +goose Up
-- +goose StatementBegin

-- Org-level SCM credentials. One row per (provider, host) pair.
-- Resolution: when a fetcher (or auto-register) needs a token to
-- hit the provider API, it first looks at scm_source.auth_ref
-- (per-project override); on miss, it falls back to this table
-- keyed by (scm_source.provider, host-from-scm_source.url).
--
-- Why per-host and not per-project: teams binding N repos in the
-- same GitLab org want to paste the PAT once, not N times. Same
-- pattern GitHub gets for free via App installations; this gives
-- GitLab + Bitbucket the equivalent.
--
-- auth_ref_encrypted is BYTEA ciphertext sealed with the server
-- cipher (GOCDNEXT_SECRET_KEY). Plaintext lives in memory only
-- during fetch/dispatch. api_base is optional — empty means the
-- provider default (gitlab.com, bitbucket.org, etc.); populate
-- for self-hosted GitLab CE/EE where the API host differs from
-- the clone URL host.
--
-- github isn't in the CHECK — GitHub has its own App-based
-- credential model in vcs_integrations. Adding a github row
-- here would create two sources of truth.
CREATE TABLE scm_credentials (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider            TEXT NOT NULL CHECK (provider IN ('gitlab', 'bitbucket')),
    host                TEXT NOT NULL,
    api_base            TEXT NOT NULL DEFAULT '',
    display_name        TEXT NOT NULL DEFAULT '',
    auth_ref_encrypted  BYTEA NOT NULL,
    created_by          UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, host)
);

-- Reverse lookup for the resolver hot path: "which credential
-- covers this (provider, host)?". Small table, but index keeps
-- the query plan stable as rows grow.
CREATE INDEX idx_scm_credentials_lookup ON scm_credentials(provider, host);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS scm_credentials;
-- +goose StatementEnd
