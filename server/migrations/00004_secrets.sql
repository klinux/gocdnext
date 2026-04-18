-- +goose Up
-- +goose StatementBegin

-- Project-scoped key/value secret store. Values are AES-256-GCM encrypted
-- at the app layer (see server/internal/crypto) before they touch the DB.
-- The ciphertext blob is nonce||ct+tag, 28 bytes + len(plaintext).
--
-- Secrets are referenced by name from jobs via the YAML `secrets:` list;
-- the scheduler resolves them to plaintext when assembling a JobAssignment
-- and the runner injects them as env vars. Nothing writes the value to
-- logs, the definition JSON, or cause_detail — those paths can't leak it.
CREATE TABLE secrets (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    value_enc    BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS secrets;
-- +goose StatementEnd
