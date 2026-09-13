-- +goose NO TRANSACTION

-- +goose Up

-- Public badges are opt-in per project. Store only a hash of the badge token:
-- the plaintext token is shown once on rotation and then lives only in the
-- README/wiki URL. NULL means badges disabled, so private projects do not leak
-- status through the anonymous endpoint unless a maintainer explicitly enables
-- them.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS badge_token_hash TEXT;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'projects'::regclass
          AND conname = 'projects_badge_token_hash_chk'
    ) THEN
        ALTER TABLE projects
            ADD CONSTRAINT projects_badge_token_hash_chk
            CHECK (badge_token_hash IS NULL OR badge_token_hash ~ '^[0-9a-f]{64}$')
            NOT VALID;
    END IF;
END $$;
-- +goose StatementEnd

COMMENT ON COLUMN projects.badge_token_hash IS
    'SHA-256 hex digest of the project badge token. NULL disables anonymous badges.';

-- Badge requests resolve the project by slug + token, then ask for the latest
-- run across either one pipeline or all project pipelines. These indexes keep
-- README/wiki traffic on bounded lookups instead of table scans.
CREATE INDEX CONCURRENTLY IF NOT EXISTS runs_badge_latest_idx
    ON runs (pipeline_id, created_at DESC, id DESC);

CREATE INDEX CONCURRENTLY IF NOT EXISTS runs_badge_latest_ref_idx
    ON runs (pipeline_id, ref, created_at DESC, id DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS runs_badge_latest_ref_idx;
DROP INDEX CONCURRENTLY IF EXISTS runs_badge_latest_idx;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_badge_token_hash_chk;
ALTER TABLE projects DROP COLUMN IF EXISTS badge_token_hash;
