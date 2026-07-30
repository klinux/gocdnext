-- name: UpsertGithubCheckRun :exec
-- Called right after CreateCheckRun on GitHub responds; caller may
-- already have an entry from a previous retry so we upsert rather
-- than insert. updated_at bumps so we can spot stale rows later.
-- completed is forced FALSE: a (re)created check run is open again, so a
-- rerun that recreates the check resets the lifecycle flag.
-- check_run_id is NULL in commit_status mode (no GitHub Check Run exists);
-- reporting_mode is the per-run effective mode, persisted so complete/reopen
-- read it back instead of re-deriving from the project's current setting.
INSERT INTO github_check_runs (
    run_id, installation_id, check_run_id, owner, repo, head_sha, status_context, reporting_mode, completed
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, FALSE)
ON CONFLICT (run_id) DO UPDATE SET
    installation_id = EXCLUDED.installation_id,
    check_run_id    = EXCLUDED.check_run_id,
    owner           = EXCLUDED.owner,
    repo            = EXCLUDED.repo,
    head_sha        = EXCLUDED.head_sha,
    status_context  = EXCLUDED.status_context,
    reporting_mode  = EXCLUDED.reporting_mode,
    completed       = FALSE,
    updated_at      = NOW();

-- name: MarkGithubCheckRunCompleted :exec
-- Flips the link's lifecycle flag after the check is PATCHed to a terminal
-- state on GitHub. A later rerun reads this to decide reuse vs. recreate:
-- GitHub won't cleanly reopen a completed check (completed_at is set-once), so
-- a completed link forces a fresh check run on the next reopen.
UPDATE github_check_runs
SET completed = TRUE, updated_at = NOW()
WHERE run_id = $1;

-- name: GetGithubCheckRun :one
-- Reporter needs owner/repo/check_run_id to patch a check when the
-- run finishes. Returns ErrNoRows when the run didn't produce a
-- check (most common path: no App installed, or feature disabled).
SELECT run_id, installation_id, check_run_id, owner, repo, head_sha,
       status_context, reporting_mode, completed, created_at, updated_at
FROM github_check_runs
WHERE run_id = $1;
