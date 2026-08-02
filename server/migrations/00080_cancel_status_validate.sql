-- +goose Up

-- #207, online split (part 2/2): VALIDATE the constraints 00079 added NOT VALID.
-- Runs in its OWN goose transaction, AFTER 00079 committed and released its ACCESS
-- EXCLUSIVE locks. VALIDATE CONSTRAINT scans the table under SHARE UPDATE
-- EXCLUSIVE — concurrent INSERT/UPDATE/DELETE/SELECT keep running; only DDL/VACUUM
-- wait. Every existing row already satisfies the widened predicates (the old value
-- sets are subsets, and cancel_origin starts NULL), so VALIDATE cannot fail on live
-- data. On a large job_runs the scan is O(rows) but non-blocking to traffic.
ALTER TABLE job_runs VALIDATE CONSTRAINT job_runs_cancel_origin_check;
ALTER TABLE deployment_revisions VALIDATE CONSTRAINT deployment_revisions_status_check;

-- +goose Down

-- No-op: 00079's Down drops both constraints entirely, so there is nothing to
-- "un-validate" here. Leaving a validated constraint in place across a partial
-- rollback is harmless (it still holds on the data).
SELECT 1;
