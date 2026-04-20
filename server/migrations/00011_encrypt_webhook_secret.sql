-- +goose Up
-- +goose StatementBegin

-- Per-repo webhook secrets are now stored encrypted under the same
-- GOCDNEXT_SECRET_KEY cipher used for /secrets, auth_providers and
-- vcs_integrations. No prod data exists yet (the global
-- GOCDNEXT_WEBHOOK_TOKEN was the fallback for everything), so a
-- destructive DROP + re-ADD is safe — any dev row with a plaintext
-- secret will need the repo reconfigured anyway once the new API
-- shape rolls out.
ALTER TABLE scm_sources DROP COLUMN IF EXISTS webhook_secret;
ALTER TABLE scm_sources ADD COLUMN webhook_secret BYTEA;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE scm_sources DROP COLUMN IF EXISTS webhook_secret;
ALTER TABLE scm_sources ADD COLUMN webhook_secret TEXT;
-- +goose StatementEnd
