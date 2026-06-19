-- +goose Up
-- +goose StatementBegin
-- External secret backends (#54): a secret entry is either a DB-stored
-- encrypted value (source='db', today's model) or a pointer into an
-- external store (source in vault|gcp|aws) holding ref_path[/ref_key] and
-- NO value. The CHECK enforces the shape at the DB edge so neither half
-- can be malformed. DEFAULT 'db' makes every existing row valid with zero
-- backfill, and the `source <> 'db'` branch already admits future
-- backends (gcp/aws) with no further migration.
ALTER TABLE secrets
    ADD COLUMN source   TEXT NOT NULL DEFAULT 'db',
    ADD COLUMN ref_path TEXT,
    ADD COLUMN ref_key  TEXT;

ALTER TABLE secrets ALTER COLUMN value_enc DROP NOT NULL;

ALTER TABLE secrets
    ADD CONSTRAINT secrets_source_shape CHECK (
        (source = 'db'  AND value_enc IS NOT NULL AND ref_path IS NULL AND ref_key IS NULL)
        OR
        (source <> 'db' AND value_enc IS NULL     AND ref_path IS NOT NULL)
    );
-- ref_key stays nullable on purpose: AWS "whole secret" and a "latest"
-- version want an empty key. Only ref_path is mandatory for external rows.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM secrets WHERE source <> 'db';
ALTER TABLE secrets DROP CONSTRAINT IF EXISTS secrets_source_shape;
ALTER TABLE secrets ALTER COLUMN value_enc SET NOT NULL;
ALTER TABLE secrets DROP COLUMN IF EXISTS ref_key;
ALTER TABLE secrets DROP COLUMN IF EXISTS ref_path;
ALTER TABLE secrets DROP COLUMN IF EXISTS source;
-- +goose StatementEnd
