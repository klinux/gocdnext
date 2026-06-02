-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin

-- The dispatch tick reads job_runs by run_id every time it considers
-- a queued run (drainQueued loop + per-completion NOTIFY). Without an
-- index on (run_id, ...) the planner falls back to a seq scan of
-- job_runs, which scales with cumulative history not active workload
-- — a long-lived cluster with hundreds of thousands of historical
-- job_runs would pay O(table) per tick.
--
-- Covering index on (run_id, name, matrix_key) with INCLUDE(status):
--
--   * Matches the predicate of ListJobStatusForRun and
--     ListJobRunsByRun (WHERE run_id = $1 ORDER BY name,
--     matrix_key NULLS FIRST). The B-tree leaves are already sorted
--     in the query's natural order, so the executor skips a Sort node
--     entirely.
--   * INCLUDE(status) makes the index covering for the lean
--     ListJobStatusForRun projection — the heap isn't touched at all
--     on hot ticks, the planner returns from the index alone
--     (Index Only Scan).
--   * NULLS FIRST matches the SQL ORDER BY and the store-side
--     iteration order so a single (name, "") entry sorts before
--     ("name", "matrix-key") siblings.
--
-- Estimated cost: ~32 bytes per row + tree overhead. On 1M
-- job_runs ≈ 32MB index. Acceptable for the perf gain on every
-- dispatch tick.
--
-- CONCURRENTLY is required here: this migration runs against
-- live clusters with active job_runs writes (agents reporting
-- results, scheduler dispatch). A normal CREATE INDEX takes an
-- ACCESS EXCLUSIVE lock for the entire build — even tens of
-- seconds of write-block on a hot table would back up agent
-- writes long enough to trip session timeouts. CONCURRENTLY
-- builds in two passes without blocking writes; trade-off is
-- ~2x build time vs the locked path, which we don't care about
-- here because the operator runs it once at deploy. The
-- NO TRANSACTION directive at the top of this file is mandatory
-- — CONCURRENTLY can't run inside a transaction block.
--
-- IF NOT EXISTS handles the rerun case: if a previous attempt
-- failed mid-build (CONCURRENTLY leaves the index in an INVALID
-- state on failure), a follow-up apply skips creation — operator
-- must REINDEX or DROP+recreate manually before the migration
-- can complete cleanly.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_job_runs_run_id
    ON job_runs (run_id, name, matrix_key NULLS FIRST)
    INCLUDE (status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_job_runs_run_id;
-- +goose StatementEnd
