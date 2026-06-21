-- +goose Up
-- +goose StatementBegin
-- definition_raw stores the pipeline as parsed from the repo YAML, BEFORE any
-- compliance policy merge. `definition` stays the EFFECTIVE (post-merge)
-- snapshot that materialisation and dispatch already read — so no downstream
-- code changes. Keeping raw lets the server recompute the effective definition
-- when a policy/framework changes, without re-fetching from the repo.
--
-- Existing pipelines carry no policy, so raw == effective: backfill from
-- definition, then enforce NOT NULL.
ALTER TABLE pipelines ADD COLUMN definition_raw JSONB;
UPDATE pipelines SET definition_raw = definition WHERE definition_raw IS NULL;
ALTER TABLE pipelines ALTER COLUMN definition_raw SET NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE pipelines DROP COLUMN definition_raw;
-- +goose StatementEnd
