-- +goose Up

-- Tracks the GitHub Check Runs we've created for a gocdnext run, so we
-- can update the same check later when the run terminates. Kept
-- separate from `runs` because:
--   - not every run produces a check (cause != webhook/pull_request,
--     or repo has no App installation, or App is not configured)
--   - check_run_id + installation_id + owner/repo are all GitHub-
--     specific context; polluting the generic `runs` table with them
--     would pull a provider shape into the core domain.
CREATE TABLE github_check_runs (
    run_id          UUID PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL,
    check_run_id    BIGINT NOT NULL,
    owner           TEXT   NOT NULL,
    repo            TEXT   NOT NULL,
    head_sha        TEXT   NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_github_check_runs_check_id
    ON github_check_runs(check_run_id);

-- +goose Down
DROP TABLE IF EXISTS github_check_runs;
