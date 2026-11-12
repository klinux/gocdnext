-- +goose Up

-- Flip the default GitHub check reporting mode for NEW projects from 'both' to
-- 'commit_status' (Woodpecker/GoCD style — a single status posted straight to
-- the commit, no duplicate Check Run). 'both' double-reported every run (a rich
-- Check Run AND a legacy Commit Status), doubling GitHub API writes and showing
-- two entries on the PR, which is wasteful and confusing. commit_status is the
-- mode our forge model is built around (Woodpecker's GitHub forge only ever
-- calls the Commit Status API, never Checks), and it keeps branch-protection
-- required checks working: they match on the status CONTEXT NAME, which
-- commit_status still posts (only check_run-only would strand them).
--
-- Scope: this changes the column DEFAULT only, so it affects projects created
-- AFTER this migration. EXISTING projects keep whatever value their row already
-- holds — switching them is a deliberate per-project action in settings (or an
-- operator UPDATE), never a silent flip on upgrade (gocdnext is shipped publicly;
-- a backfill here would change every deployment's existing projects).
--
-- github_check_runs.reporting_mode is intentionally left at its 'both' default:
-- every INSERT sets reporting_mode explicitly from the project's mode at
-- dispatch time, so that column default is never observed in practice.
ALTER TABLE projects
    ALTER COLUMN check_reporting_mode SET DEFAULT 'commit_status';

-- +goose Down
ALTER TABLE projects
    ALTER COLUMN check_reporting_mode SET DEFAULT 'both';
