-- +goose Up
-- +goose StatementBegin

-- Platform-wide runtime settings the operator can change without
-- editing the chart values + restarting through GitOps. Generic
-- key/value table so future settings (SCM defaults, retention
-- overrides, etc) reuse the same row shape without per-feature
-- migrations.
--
-- The contract:
--   key             stable string key, app-defined namespacing
--                   (e.g. 'artifacts.storage')
--   value           non-secret config — bucket name, region,
--                   endpoint, flags. Plain JSONB so the admin UI
--                   can read/write it directly.
--   credentials_enc AEAD-sealed blob holding the secret half
--                   (access keys, service-account JSON). Same
--                   cipher used by project secrets / runner-
--                   profile secrets / auth providers.
--                   NULL when the setting carries no credentials
--                   (e.g. a filesystem backend).
--
-- Single-row keyed by string — no sequences, no UUID. Cheap to
-- read on boot, easy to mutate from the admin UI, and the schema
-- doesn't grow new columns when a new setting type lands.
CREATE TABLE platform_settings (
    key             TEXT PRIMARY KEY,
    value           JSONB NOT NULL,
    credentials_enc BYTEA,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by      UUID REFERENCES users(id) ON DELETE SET NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS platform_settings;

-- +goose StatementEnd
