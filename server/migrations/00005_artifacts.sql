-- +goose Up

-- Replace the Phase-0 stub in 00001_init.sql with the real schema. The
-- stub was never read from or written to (grep `storage_url` on the repo
-- to verify), so dropping is safe; no data migration is needed.
DROP TABLE IF EXISTS artifacts;

-- Artifacts uploaded by jobs. The `storage_key` is opaque (UUID) and
-- points at an object in whatever `artifacts.Store` backend is configured
-- (filesystem, S3, GCS). The server is authoritative for metadata; the
-- backend stores raw bytes.
--
-- Lifecycle:
--   pending  — row created when agent requests an upload URL
--   ready    — server confirmed the object is in the backend (HEAD ok)
--   deleting — sweeper marked for removal (idempotent re-try on restart)
--
-- Retention columns (project_id, pipeline_id, pinned_at, deleted_at)
-- back the sweeper layers documented in docs/artifacts-design.md:
--   1. TTL:         expires_at < now()
--   2. Keep-last:   per pipeline, prune runs beyond N
--   3. Project cap: LRU by project
--   4. Global cap:  LRU overall
-- `pinned_at IS NOT NULL` exempts the row from all four layers.
CREATE TABLE artifacts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id         UUID NOT NULL REFERENCES runs(id)      ON DELETE CASCADE,
    job_run_id     UUID NOT NULL REFERENCES job_runs(id)  ON DELETE CASCADE,
    pipeline_id    UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    project_id     UUID NOT NULL REFERENCES projects(id)  ON DELETE CASCADE,
    path           TEXT   NOT NULL,
    storage_key    TEXT   NOT NULL UNIQUE,
    size_bytes     BIGINT NOT NULL DEFAULT 0,
    content_sha256 TEXT   NOT NULL DEFAULT '',
    status         TEXT   NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','ready','deleting')),
    pinned_at      TIMESTAMPTZ,
    deleted_at     TIMESTAMPTZ,
    expires_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_artifacts_run     ON artifacts(run_id);
CREATE INDEX idx_artifacts_job_run ON artifacts(job_run_id);

CREATE INDEX idx_artifacts_ttl
    ON artifacts(expires_at)
    WHERE expires_at IS NOT NULL AND deleted_at IS NULL AND pinned_at IS NULL;

CREATE INDEX idx_artifacts_project_lru
    ON artifacts(project_id, created_at)
    WHERE deleted_at IS NULL AND pinned_at IS NULL;

CREATE INDEX idx_artifacts_pipeline_lru
    ON artifacts(pipeline_id, created_at)
    WHERE deleted_at IS NULL AND pinned_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS artifacts;
