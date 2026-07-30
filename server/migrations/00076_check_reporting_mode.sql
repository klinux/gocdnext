-- +goose Up

-- Per-project control over how gocdnext reports run state to GitHub:
--   both          – Check Run + legacy Commit Status (default; today's behavior)
--   check_run     – only the rich Check Run
--   commit_status – only the straight-to-run Commit Status (Woodpecker/GoCD style)
-- Default 'both' preserves current behavior AND keeps existing branch-protection
-- required-checks working (they point at the commit status context today);
-- switching away is a deliberate per-project opt-out.
ALTER TABLE projects
    ADD COLUMN check_reporting_mode TEXT NOT NULL DEFAULT 'both'
        CHECK (check_reporting_mode IN ('both', 'check_run', 'commit_status'));

-- The github_check_runs row is the run's GitHub-reporting IDENTITY in EVERY mode
-- (owner/repo/head_sha/status_context are read back on terminal/reopen). In
-- commit_status mode there is no Check Run, so check_run_id becomes a real
-- nullable (never a 0 sentinel). reporting_mode is persisted per-run so a mid-run
-- settings flip can't strand the reporting — complete/reopen/security read it
-- back from the row, never re-deriving from the project's current setting.
ALTER TABLE github_check_runs
    ALTER COLUMN check_run_id DROP NOT NULL;

ALTER TABLE github_check_runs
    ADD COLUMN reporting_mode TEXT NOT NULL DEFAULT 'both'
        CHECK (reporting_mode IN ('both', 'check_run', 'commit_status'));

-- The lookup index only ever queries non-NULL check_run_ids; make it partial so
-- commit_status-only rows (NULL) don't bloat it.
DROP INDEX IF EXISTS idx_github_check_runs_check_id;
CREATE INDEX idx_github_check_runs_check_id
    ON github_check_runs(check_run_id) WHERE check_run_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_github_check_runs_check_id;
CREATE INDEX idx_github_check_runs_check_id ON github_check_runs(check_run_id);
ALTER TABLE github_check_runs DROP COLUMN reporting_mode;
-- commit_status-only rows carry a NULL check_run_id — the pre-migration code
-- can't operate a reporting identity without a Check Run, so drop them before
-- restoring NOT NULL (otherwise the ALTER fails on the existing NULLs). These
-- rows only gate rerun reuse-vs-recreate; losing them just means the next run
-- re-reports from scratch.
DELETE FROM github_check_runs WHERE check_run_id IS NULL;
ALTER TABLE github_check_runs ALTER COLUMN check_run_id SET NOT NULL;
ALTER TABLE projects DROP COLUMN check_reporting_mode;
