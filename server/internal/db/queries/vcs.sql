-- VCS pull-request lifecycle (the #112 epic). Webhook handlers upsert the
-- opened / approved / merged timestamps as events arrive in any order; one row
-- per (provider, repo, number). Feeds DORA lead-time decomposition later.

-- name: UpsertPullRequestOpened :exec
-- Recorded on pull_request opened/reopened/synchronize. opened_at keeps its
-- first (created_at) value; head/refs/title follow the latest event.
INSERT INTO vcs_pull_requests (
    provider, repo, number, title, author, head_ref, base_ref, head_sha, opened_at, updated_at
) VALUES (
    @provider, @repo, @number, @title, @author, @head_ref, @base_ref, @head_sha, @opened_at, now()
)
ON CONFLICT (provider, repo, number) DO UPDATE SET
    title     = EXCLUDED.title,
    author    = EXCLUDED.author,
    head_ref  = EXCLUDED.head_ref,
    base_ref  = EXCLUDED.base_ref,
    head_sha  = EXCLUDED.head_sha,
    -- LEAST ignores NULLs → the EARLIEST opened_at wins regardless of which
    -- webhook arrived first (deliveries can be reordered/retried).
    opened_at = LEAST(vcs_pull_requests.opened_at, EXCLUDED.opened_at),
    updated_at = now();

-- name: SetPullRequestFirstCommit :exec
-- Recorded from the provider commits API when a PR opens. first_commit_at keeps
-- the earliest (the start of the Coding stage).
INSERT INTO vcs_pull_requests (provider, repo, number, first_commit_at, updated_at)
VALUES (@provider, @repo, @number, @first_commit_at, now())
ON CONFLICT (provider, repo, number) DO UPDATE SET
    first_commit_at = LEAST(vcs_pull_requests.first_commit_at, EXCLUDED.first_commit_at),
    updated_at      = now();

-- name: MarkPullRequestApproved :exec
-- Recorded on the first approving review. approved_at keeps the earliest.
INSERT INTO vcs_pull_requests (provider, repo, number, approved_at, updated_at)
VALUES (@provider, @repo, @number, @approved_at, now())
ON CONFLICT (provider, repo, number) DO UPDATE SET
    -- Earliest approval wins by submitted_at, not by delivery order.
    approved_at = LEAST(vcs_pull_requests.approved_at, EXCLUDED.approved_at),
    updated_at  = now();

-- name: MarkPullRequestMerged :exec
-- Recorded on pull_request closed with merged=true. merge_sha is the commit
-- that landed on the base branch (correlates to a deployment in phase 2).
INSERT INTO vcs_pull_requests (provider, repo, number, merge_sha, merged_at, closed_at, updated_at)
VALUES (@provider, @repo, @number, @merge_sha, @merged_at, @closed_at, now())
ON CONFLICT (provider, repo, number) DO UPDATE SET
    merge_sha = CASE WHEN EXCLUDED.merge_sha <> '' THEN EXCLUDED.merge_sha
                     ELSE vcs_pull_requests.merge_sha END,
    merged_at = LEAST(vcs_pull_requests.merged_at, EXCLUDED.merged_at),
    closed_at = LEAST(vcs_pull_requests.closed_at, EXCLUDED.closed_at),
    updated_at = now();

-- name: GetPullRequest :one
SELECT * FROM vcs_pull_requests
WHERE provider = @provider AND repo = @repo AND number = @number;
