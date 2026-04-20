-- +goose Up
-- +goose StatementBegin

-- Server-wide VCS integration config, admin-managed from
-- /settings/integrations. Scope: per-control-plane credentials for
-- talking TO a VCS provider (GitHub App today). NOT per-project
-- repo binding — that's what scm_sources is for.
--
-- private_key and webhook_secret are AES-GCM ciphertext sealed
-- with GOCDNEXT_SECRET_KEY (the same cipher used for /secrets and
-- auth_providers). A DB dump without the key gives nothing.
--
-- kind is constrained to values the registry knows how to
-- instantiate. Adding another (gitlab_oauth, bitbucket_app) is a
-- CHECK-constraint migration + a new instantiateFromRow branch in
-- the registry — same escape hatch pattern we used for auth_providers.
CREATE TABLE vcs_integrations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            TEXT NOT NULL,
    name            TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL DEFAULT '',
    app_id          BIGINT,
    private_key     BYTEA,
    webhook_secret  BYTEA,
    api_base        TEXT NOT NULL DEFAULT '',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT vcs_integrations_kind_check CHECK (kind IN ('github_app'))
);

CREATE INDEX idx_vcs_integrations_kind ON vcs_integrations (kind);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS vcs_integrations;
-- +goose StatementEnd
