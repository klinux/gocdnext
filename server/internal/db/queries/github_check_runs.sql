-- name: UpsertGithubCheckRun :exec
-- Called right after CreateCheckRun on GitHub responds; caller may
-- already have an entry from a previous retry so we upsert rather
-- than insert. updated_at bumps so we can spot stale rows later.
INSERT INTO github_check_runs (
    run_id, installation_id, check_run_id, owner, repo, head_sha
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (run_id) DO UPDATE SET
    installation_id = EXCLUDED.installation_id,
    check_run_id    = EXCLUDED.check_run_id,
    owner           = EXCLUDED.owner,
    repo            = EXCLUDED.repo,
    head_sha        = EXCLUDED.head_sha,
    updated_at      = NOW();

-- name: GetGithubCheckRun :one
-- Reporter needs owner/repo/check_run_id to patch a check when the
-- run finishes. Returns ErrNoRows when the run didn't produce a
-- check (most common path: no App installed, or feature disabled).
SELECT run_id, installation_id, check_run_id, owner, repo, head_sha,
       created_at, updated_at
FROM github_check_runs
WHERE run_id = $1;
