-- +goose Up
-- +goose StatementBegin

-- Rolling-upgrade safety: the old server pod still serves
-- RequestArtifactUpload traffic while the new pod runs this
-- migration (kubernetes drains old replicas only after the new
-- one passes readiness). Without this LOCK, old-pod inserts
-- between the dedupe step (#2) and the CREATE UNIQUE INDEX could
-- plant fresh duplicates the index then refuses, aborting the
-- migration and trapping the operator in a half-upgraded cluster.
--
-- SHARE MODE is the minimum lock that blocks INSERT/UPDATE/DELETE
-- while still allowing SELECT — the old pod's reads (UI artifact
-- list, downstream job lookups) keep working through the (brief)
-- migration window. Inserts queue and resume after COMMIT; any
-- queued insert that WOULD have been a duplicate then fails with
-- a unique_violation, which the gRPC handler converts to Internal
-- and the agent fails the job loudly. Strictly better than the
-- pre-fix silent corruption: the failure surface is bounded to
-- whichever upload races the upgrade window.
--
-- Goose runs each migration in its own transaction by default, so
-- this LOCK is held until COMMIT at the end of the file. The whole
-- migration is bounded by the table size of `artifacts` — small in
-- practice (the sweeper keeps it trimmed), so the window is sub-
-- second on any realistic deployment.
LOCK TABLE artifacts IN SHARE MODE;

-- Backfill step 1: canonicalize path.
--
-- The application now trims trailing slashes on InsertPendingArtifact
-- so future rows land in canonical form (`dist`, not `dist/`). Older
-- rows still carry the operator-typed shape. Without this backfill,
-- a post-upgrade lookup with `dist/` normalizes to `dist` and would
-- 0-row against the legacy `dist/` rows still in the table.
--
-- Idempotent: regexp_replace on a path with no trailing slash is a
-- no-op. The trailing-slash-only path '/' is preserved as-is (rare
-- but legal — operator might want to ship "everything"). The
-- WHERE filter keeps this fast: only rows that NEED rewriting are
-- touched, not the whole table.
UPDATE artifacts
SET path = regexp_replace(path, '/+$', '')
WHERE path ~ '/$' AND path <> '/';

-- Backfill step 2: retire duplicate active rows BEFORE the unique
-- index lands.
--
-- A DB that hit issue #3 (reaper requeue without retiring artifacts)
-- now has multiple `deleted_at IS NULL` rows per (job_run_id, path).
-- Creating the unique index naively would fail with "duplicate key
-- value" and abort the migration, blocking the upgrade entirely on
-- exactly the deployments the bug already affected.
--
-- Ranking preserves the row most likely to be useful:
--   1. Pinned rows first (operator explicitly marked it important).
--   2. Then ready rows over pending (a confirmed object beats a
--      half-uploaded one).
--   3. Within a category, newest by created_at (the most recent
--      attempt's row).
-- All non-survivor rows get the same retire treatment the runtime
-- now applies on reclaim: status='deleting', deleted_at=NOW,
-- expires_at=NOW. The sweeper's deleting-status branch picks them
-- up on its next pass, removing both the row and (where present)
-- the storage object.
--
-- Idempotent: if the migration is re-run on a clean DB, the CTE
-- finds rn=1 only and no rows are updated.
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY job_run_id, path
               ORDER BY
                   CASE WHEN pinned_at  IS NOT NULL THEN 0 ELSE 1 END,
                   CASE WHEN status = 'ready'        THEN 0 ELSE 1 END,
                   created_at DESC
           ) AS rn
    FROM artifacts
    WHERE deleted_at IS NULL
)
UPDATE artifacts
SET status     = 'deleting',
    deleted_at = NOW(),
    expires_at = NOW(),
    -- Clear pinned_at on retired duplicates: the sweeper skips
    -- rows with pinned_at IS NOT NULL, so a pinned duplicate that
    -- the migration retires would otherwise sit `status='deleting'`,
    -- `deleted_at IS NOT NULL`, `pinned_at IS NOT NULL` forever —
    -- invisible to the lookup AND immune to the sweeper, with its
    -- storage object orphaned in S3. Matches the runtime
    -- RetireArtifactsByJobRun + RerunJob behaviour: a retired
    -- duplicate is a dead row, the pin has no business surviving.
    pinned_at  = NULL
WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

-- Partial unique index on (job_run_id, path) for active rows.
--
-- Schema-layer defense against the bug class: a future regression
-- in requeueStaleJob / RerunJob that forgets to retire artifacts
-- now fails the INSERT loudly instead of silently producing
-- duplicates. The partial predicate (deleted_at IS NULL) lets the
-- soft-deleted prior rows coexist with the new attempt's inserts
-- until the sweeper picks them up.
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_jobrun_path_active
    ON artifacts (job_run_id, path)
    WHERE deleted_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_artifacts_jobrun_path_active;
-- Down does NOT un-trim paths or un-retire duplicates — those are
-- one-way data-shape fixes. Re-running Up after a Down is still
-- idempotent because the CTE finds no duplicates and the regexp
-- no-ops on canonical paths.
-- +goose StatementEnd
