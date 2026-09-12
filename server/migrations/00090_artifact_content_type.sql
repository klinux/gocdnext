-- +goose Up

-- Persist the artifact's compression codec as a content type so the manual
-- UI/API download path can name the file and set the header correctly
-- (#283). Job→job restore already auto-detects via magic bytes; downloads
-- did not, so a zstd artifact would download as `.tar.gz` and fail `tar xzf`.
--
-- Additive + online: NOT NULL with a constant DEFAULT is a metadata-only
-- change in Postgres 11+ (no table rewrite), and every existing row reads
-- back as gzip — which is exactly what they are (the artifact store only
-- ever wrote gzip before the zstd codec landed). The server overwrites this
-- with the codec it SNIFFS from the object bytes at mark-ready, so the value
-- is server-observed, never trusted from the agent.
ALTER TABLE artifacts
    ADD COLUMN content_type TEXT NOT NULL DEFAULT 'application/gzip';

-- +goose Down
ALTER TABLE artifacts DROP COLUMN content_type;
