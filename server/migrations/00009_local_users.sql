-- +goose Up
-- +goose StatementBegin

-- Local (password-based) users. Layered on top of the existing
-- OIDC-driven users table: adding a nullable column keeps OIDC
-- users intact (password_hash IS NULL) while opening up a
-- separate sign-in path for the `local` provider.
--
-- Scope: this is intentionally a break-glass / bootstrap
-- mechanism, not a primary auth surface. See docs/auth.md (to be
-- written) for policy:
--   - provisioned via the `gocdnext admin create-user` CLI
--   - no email verification, no password-reset-by-email, no MFA
--   - a login form renders on /login only when at least one
--     local user exists
--
-- For local users, external_id == email (the unique key pair
-- (provider, external_id) stays honored with provider='local').
ALTER TABLE users ADD COLUMN password_hash BYTEA;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN password_hash;
-- +goose StatementEnd
