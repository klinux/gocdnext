-- +goose Up

-- #207 — a canceled running job must record `canceled`, not `failed`. Two schema
-- changes support the server-side derivation:
--
--   (1) job_runs.cancel_origin — WHO/WHAT requested the cancel, so an upstream
--       rerun can tell a user's deliberate single-job cancel (never resurrect it)
--       from a system/dependency cancel (revive it). Nullable: pre-existing rows
--       carry NULL and the revival predicate uses IS DISTINCT FROM, so a NULL
--       origin still revives (today's behaviour).
--
--   (2) deployment_revisions.status gains 'canceled', so a canceled deploy records
--       a canceled revision (never becomes current, stays in history, excluded
--       from DORA — the analytics queries already filter status IN
--       ('success','failed')).
--
-- ONLINE SPLIT: this migration does ONLY instant DDL — ADD COLUMN (nullable, no
-- default → metadata-only), and ADD/DROP CONSTRAINT ... NOT VALID (no table scan).
-- goose wraps it in one transaction, so the brief ACCESS EXCLUSIVE locks are held
-- only for these instant statements and released at COMMIT. The row scan is
-- deferred to 00080 (VALIDATE), a SEPARATE transaction that takes the weaker
-- SHARE UPDATE EXCLUSIVE — so the expensive part never holds ACCESS EXCLUSIVE.
-- Keeping the DROP+ADD of the deployment_revisions CHECK transactional here means
-- a failure rolls the table back to its old constraint (never constraint-less).

ALTER TABLE job_runs ADD COLUMN cancel_origin TEXT;

ALTER TABLE job_runs
    ADD CONSTRAINT job_runs_cancel_origin_check
    CHECK (cancel_origin IS NULL OR cancel_origin IN
        ('user_job', 'user_run', 'supersede', 'approval_expiry', 'dependency'))
    NOT VALID;

-- Widen the deployment_revisions status set. The inline column CHECK from 00046 is
-- auto-named `deployment_revisions_status_check`; drop it and re-add the widened
-- predicate NOT VALID. Atomic with the drop (same tx) so the table is never left
-- constraint-less on a failure.
ALTER TABLE deployment_revisions DROP CONSTRAINT deployment_revisions_status_check;
ALTER TABLE deployment_revisions
    ADD CONSTRAINT deployment_revisions_status_check
    CHECK (status IN ('in_progress', 'success', 'failed', 'canceled'))
    NOT VALID;

-- +goose Down

-- A deployment_revision is audit history: CONVERT canceled → failed before
-- restoring the narrower CHECK (never DELETE — losing the record would rewrite the
-- deploy timeline). failed is the closest pre-#207 terminal for a non-success deploy.
UPDATE deployment_revisions SET status = 'failed' WHERE status = 'canceled';
ALTER TABLE deployment_revisions DROP CONSTRAINT deployment_revisions_status_check;
ALTER TABLE deployment_revisions
    ADD CONSTRAINT deployment_revisions_status_check
    CHECK (status IN ('in_progress', 'success', 'failed'));

ALTER TABLE job_runs DROP CONSTRAINT job_runs_cancel_origin_check;
ALTER TABLE job_runs DROP COLUMN cancel_origin;
