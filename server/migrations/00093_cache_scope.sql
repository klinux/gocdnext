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
--
-- ROLLOUT NOTE (expand/contract): this swap is NOT online-safe against a
-- still-running pre-scope binary, which upserts with ON CONFLICT
-- (project_id, key). During a RollingUpdate the old pod briefly sees the
-- new schema and its cache PUT/GET degrade — but the cache path is
-- fail-open on the agent (a PUT/GET error is logged and the build runs
-- cold, never fails), so the blast radius is "some cold builds for the
-- ~minute of overlap", self-healing on the next run. The LRU index is
-- intentionally left as-is on (last_accessed_at): a (scope, ...) index
-- doesn't serve the global-LRU/quota scans or the CASE-by-scope expiry,
-- and would force a Seq Scan+Sort as the row count grows.
ALTER TABLE caches DROP CONSTRAINT IF EXISTS caches_project_id_key_key;
ALTER TABLE caches ADD CONSTRAINT caches_project_key_scope_key
    UNIQUE (project_id, key, scope);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Rollback collapses back to one row per (project_id, key). Isolated
-- lanes have no home in the 2-col unique world, so drop them first and
-- keep only the shared ('@') lane — otherwise re-adding the 2-col unique
-- fails on the duplicate (project_id, key) pairs the feature created.
DELETE FROM caches WHERE scope <> '@';
ALTER TABLE caches DROP CONSTRAINT IF EXISTS caches_project_key_scope_key;
ALTER TABLE caches ADD CONSTRAINT caches_project_id_key_key
    UNIQUE (project_id, key);
ALTER TABLE caches DROP COLUMN IF EXISTS scope;
-- +goose StatementEnd
