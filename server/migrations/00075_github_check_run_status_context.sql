-- +goose Up

-- The commit status posted alongside the check run is keyed by a stable
-- `context` string (e.g. ci/gocdnext/<project>/<pipeline>). Persist it on the
-- link so the terminal update reuses the EXACT context/identity the pending
-- status was posted with — never re-deriving from the (possibly changed or
-- removed) material at completion time, which could leave the status stuck in
-- `pending`. Empty for links created before this column existed.
ALTER TABLE github_check_runs ADD COLUMN status_context TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE github_check_runs DROP COLUMN IF EXISTS status_context;
