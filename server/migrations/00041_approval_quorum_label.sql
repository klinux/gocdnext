-- +goose Up
-- +goose StatementBegin

-- PR-label-driven approval quorum (issue follow-up): when a run is
-- triggered by a PR carrying a label that matches the job's
-- approval.quorum_by_label map, the gate's effective quorum is
-- overridden at materialisation time (NOT at gate-decision time —
-- relabel + repush is the relabel mechanism).
--
-- approval_required already holds the EFFECTIVE quorum that the
-- state-machine evaluates against. The new column records WHICH
-- label triggered the override so the UI + audit log can explain
-- "this gate is quorum 1 because the PR carries 'hotfix', not the
-- pipeline's default 2". NULL means no override fired — either the
-- run wasn't a PR, the PR had no labels, or none intersected the
-- quorum_by_label map.

ALTER TABLE job_runs
    ADD COLUMN approval_quorum_label TEXT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_runs
    DROP COLUMN approval_quorum_label;
-- +goose StatementEnd
