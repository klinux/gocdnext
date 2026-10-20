-- +goose Up
-- +goose StatementBegin

-- PR-safe cache scoping (GitHub-Actions-style ref isolation).
--
-- Until now a cache row was keyed UNIQUE (project_id, key): the whole
-- project shared one blob per key, last-writer-wins. That clobbers under
-- concurrent branches — two PRs restoring the same key, each appending
-- their own build-cache entries, the last PUT overwriting the others →
-- spurious cache misses on the shared blob.
--
-- `scope` splits the key space by ref lane so writes can't collide:
--   '@'            the shared lane — ONLY a push/integration to the
--                  project default branch writes here. Every other lane
--                  reads it as a fallback base (see GetReadyCacheByKeyScoped).
--   '<cause>/<ref>' an isolated lane per non-default ref (feature branch,
--                  pull_request pr:<N>, merge_group base, tag). A run in
--                  this lane writes only here and never touches '@'.
--
-- Default '@' so every pre-existing row keeps its current (project-wide =
-- shared-lane) meaning — the migration is a no-op for the stored data and
-- the legacy storage_key (sha256 of just the key) still matches, because
-- the '@' lane hashes identically (see cacheStorageKeyScoped).
ALTER TABLE caches ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT '@';

-- Swap the uniqueness from (project_id, key) to (project_id, key, scope)
-- so the same key can hold one blob per lane. The old constraint carries
-- Postgres's conventional inline name; dropping it is required or the
-- 2-col unique would still reject a second lane for the same key.
ALTER TABLE caches DROP CONSTRAINT IF EXISTS caches_project_id_key_key;
ALTER TABLE caches ADD CONSTRAINT caches_project_key_scope_key
    UNIQUE (project_id, key, scope);

-- The TTL sweep applies a shorter horizon to non-'@' lanes (a dead PR
-- branch's cache should surrender disk fast). Index scope alongside the
-- LRU column so the scoped expiry scan stays cheap.
DROP INDEX IF EXISTS caches_ready_lru_idx;
CREATE INDEX caches_ready_lru_idx
    ON caches (scope, last_accessed_at)
    WHERE status = 'ready';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS caches_ready_lru_idx;
CREATE INDEX caches_ready_lru_idx
    ON caches (last_accessed_at)
    WHERE status = 'ready';
ALTER TABLE caches DROP CONSTRAINT IF EXISTS caches_project_key_scope_key;
ALTER TABLE caches ADD CONSTRAINT caches_project_id_key_key
    UNIQUE (project_id, key);
ALTER TABLE caches DROP COLUMN IF EXISTS scope;
-- +goose StatementEnd
