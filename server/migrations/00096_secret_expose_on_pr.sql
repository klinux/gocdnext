-- +goose Up

-- Per-secret pull_request exposure (secret event-scoping). When a run's cause is
-- pull_request, the scheduler withholds any declared secret whose expose_on_pr is
-- false — so a PR from a branch a contributor controls can't exfiltrate a secret
-- by editing the pipeline at the PR head (the #223 PR-head-config risk), matching
-- Woodpecker's "secrets are not exposed to pull_request events by default".
--
-- Grandfather: add the column defaulting TRUE so every EXISTING secret keeps its
-- current behavior (secrets already flow to PR runs today) — a no-surprise
-- upgrade — then flip the DEFAULT to FALSE so secrets created AFTER this migration
-- are PR-closed unless explicitly opted in. Applies to project rows and global
-- (project_id IS NULL) rows alike.
ALTER TABLE secrets
    ADD COLUMN expose_on_pr BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE secrets
    ALTER COLUMN expose_on_pr SET DEFAULT false;

-- +goose Down
ALTER TABLE secrets DROP COLUMN expose_on_pr;
