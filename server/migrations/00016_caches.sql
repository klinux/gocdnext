-- +goose Up
-- +goose StatementBegin

-- Persistent per-project caches. Each row is one named cache blob:
-- key + stored tarball + metadata. Agents GET by (project_id, key)
-- before tasks run, PUT after success. Same key across runs hits
-- the same row → natural overwrite semantics.
--
-- Scope is project-wide on purpose: two pipelines in the same
-- project using the same key share the blob (the ci-web case —
-- every Node job reuses one pnpm-store). If a future operator
-- needs pipeline-level isolation, add a column then.
--
-- status: "pending" during the window between UpsertPendingCache
-- and MarkCacheReady. Readers ignore pending rows so a partial
-- upload (agent crashed mid-PUT) doesn't feed torn data to the
-- next run.
--
-- last_accessed_at feeds the future eviction sweeper
-- (see roadmap_cache_eviction memory). Every successful GET
-- bumps it; the TTL policy evicts idle caches first.
CREATE TABLE caches (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key              TEXT        NOT NULL,
    storage_key      TEXT        NOT NULL,
    size_bytes       BIGINT      NOT NULL DEFAULT 0,
    content_sha256   TEXT,
    status           TEXT        NOT NULL DEFAULT 'pending',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (project_id, key)
);

-- Eviction sweeper walks oldest-last_accessed first. Partial
-- index on status='ready' so we don't trip over in-flight
-- uploads that never completed.
CREATE INDEX caches_ready_lru_idx
    ON caches (last_accessed_at)
    WHERE status = 'ready';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS caches;
-- +goose StatementEnd
