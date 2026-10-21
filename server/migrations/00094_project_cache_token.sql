-- +goose Up
-- +goose StatementBegin

-- Embedded build cache (Gradle HttpBuildCache protocol) is opt-in per project
-- and authenticated by a per-project token. Store only a SHA-256 hash of it:
-- the plaintext is shown once on rotation and then lives only in the build's
-- credentials (a CI secret). NULL means the embedded build cache is disabled
-- for the project, so its endpoint rejects every request until a maintainer
-- generates a token. Mirrors projects.badge_token_hash (migration 00092).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS cache_token_hash TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'projects'::regclass
          AND conname = 'projects_cache_token_hash_chk'
    ) THEN
        ALTER TABLE projects
            ADD CONSTRAINT projects_cache_token_hash_chk
            CHECK (cache_token_hash IS NULL OR cache_token_hash ~ '^[0-9a-f]{64}$')
            NOT VALID;
    END IF;
END $$;

COMMENT ON COLUMN projects.cache_token_hash IS
    'SHA-256 hex digest of the project build-cache token. NULL disables the embedded build cache endpoint.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_cache_token_hash_chk;
ALTER TABLE projects DROP COLUMN IF EXISTS cache_token_hash;
-- +goose StatementEnd
