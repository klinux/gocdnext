-- +goose Up
-- +goose StatementBegin
-- deploy_rollback flags that THIS dispatch of a job is a rollback
-- (#39 phase 3). It is the channel that carries the operator's intent
-- from the rollback endpoint (which RerunJob's the deploy job of a
-- past run) through to the scheduler dispatch, where the
-- deployment_revision is opened: a rollback dispatch records the
-- revision with is_rollback=true. Set per-dispatch by RerunJob (true
-- for a rollback, false for an ordinary rerun); a fresh job_run is
-- false by default. Only deploy jobs ever read it; for everything
-- else it is inert.
ALTER TABLE job_runs ADD COLUMN deploy_rollback BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_runs DROP COLUMN deploy_rollback;
-- +goose StatementEnd
