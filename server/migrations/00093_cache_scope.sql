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
-- still-running pre-scope binary. Its cache PUT upserts with ON CONFLICT
-- (project_id, key) (now gone) and its cache GET filters only
-- (project_id, key, status) — so against the new schema the old GET can
-- match MULTIPLE lanes, return one with a valid SHA, and hand a
-- ref-scoped blob to a job that asked for the shared cache. The cache
-- path is fail-open (errors → cold build), but a wrong-but-valid blob
-- restore is NOT an error, so it bypasses fail-open: it can poison a
-- default-branch build with a PR's cache and fail tasks or change outputs.
-- Deploy this WITHOUT overlap. The chart defaults the server Deployment to
-- strategy: Recreate for exactly this reason (see values.yaml); keep it
-- until your minimum supported version is past 00093.
--
-- The LRU index is intentionally left as-is on (last_accessed_at): a
-- (scope, ...) index doesn't serve the global-LRU/quota scans or the
-- CASE-by-scope expiry, and would force a Seq Scan+Sort as rows grow.
ALTER TABLE caches DROP CONSTRAINT IF EXISTS caches_project_id_key_key;
ALTER TABLE caches ADD CONSTRAINT caches_project_key_scope_key
    UNIQUE (project_id, key, scope);

-- The retention sweeper reclaims abandoned `pending` uploads and retries
-- rows stuck in `deleting` (tombstones), scanning by updated_at. The
-- existing LRU index is partial on status='ready', so without this the
-- reclaim pass is a full-table Seq Scan + Sort every tick. Per-ref lanes
-- make pending rows accumulate; `deleting` rows are rare (only on a blob-
-- delete failure) but share the index so the retry scan stays cheap too.
CREATE INDEX caches_pending_updated_idx
    ON caches (updated_at)
    WHERE status IN ('pending', 'deleting');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- DEV-ONLY. This project is forward-only in production (see CLAUDE.md:
-- "goose, forward-only. Rollback = a corrective new migration"), so this
-- Down exists for local/test use, where cache storage is ephemeral.
--
-- It collapses back to one row per (project_id, key): isolated lanes have
-- no home in the 2-col unique world, so drop them first and keep only the
-- shared ('@') lane — otherwise re-adding the 2-col unique fails on the
-- duplicate pairs the feature created. The DELETE drops the only DB
-- pointers to those lanes' external blobs WITHOUT removing the objects,
-- so in a real deployment this would orphan them. A production rollback
-- must therefore be a corrective FORWARD migration that also purges the
-- scoped blobs at the application layer (the sweeper can't, once the rows
-- are gone) — do NOT run this Down against prod storage.
DROP INDEX IF EXISTS caches_pending_updated_idx;
DELETE FROM caches WHERE scope <> '@';
ALTER TABLE caches DROP CONSTRAINT IF EXISTS caches_project_key_scope_key;
ALTER TABLE caches ADD CONSTRAINT caches_project_id_key_key
    UNIQUE (project_id, key);
ALTER TABLE caches DROP COLUMN IF EXISTS scope;
-- +goose StatementEnd
