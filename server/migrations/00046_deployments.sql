-- +goose Up
-- +goose StatementBegin
-- Deployment primitive (#39): a TRACKING layer on top of the deploy
-- plugins (argocd/helm/kubectl/git-bump) that already do the work.
-- gocdnext does not render manifests or talk to clusters — it records
-- which version went to which environment, when, by which run.
--
-- environments: one row per deploy target per project. Lazy-created
-- the first time a job's `deploy: {environment: X}` is dispatched;
-- the description is editable afterwards from the UI.
CREATE TABLE environments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- deployment_revisions: the history. One row per deploy attempt (per
-- dispatch of a job carrying a `deploy:` block). Created `in_progress`
-- at dispatch with the resolved version; finalised to success/failed
-- on the job's terminal result.
--
-- "current version of an env" is derived, not stored: the latest row
-- WHERE status='success' ORDER BY finished_at DESC. No pointer column,
-- no superseded/rolled_back states to reconcile — a rollback is just a
-- new success row with is_rollback=true and an older version.
--
-- run_id/job_run_id are ON DELETE SET NULL (NOT cascade): the deploy
-- record is a durable audit fact that must outlive run retention. When
-- the run is garbage-collected the link breaks (UI degrades the run
-- link, and rollback — which re-runs job_run_id — is no longer offered
-- for that revision), but "prod ran version 1.40 on <date> by <who>"
-- survives. environment_id IS cascade: dropping an environment drops
-- its history with it.
-- attempt: a job_run keeps its id across retries/reaper-requeues and
-- only bumps job_runs.attempt (see store.ReclaimStaleJobs /
-- RerunJob). Every dispatch records a revision, so the (job_run_id,
-- attempt) pair — NOT job_run_id alone — identifies one deploy
-- attempt. Finalisation keys off both, otherwise a success on
-- attempt 1 would flip a stale attempt-0 in_progress row to success
-- too (two successes for one deploy).
CREATE TABLE deployment_revisions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    environment_id UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    run_id         UUID REFERENCES runs(id) ON DELETE SET NULL,
    job_run_id     UUID REFERENCES job_runs(id) ON DELETE SET NULL,
    attempt        INTEGER NOT NULL DEFAULT 0,
    version        TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'in_progress'
                       CHECK (status IN ('in_progress', 'success', 'failed')),
    is_rollback    BOOLEAN NOT NULL DEFAULT FALSE,
    deployed_by    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at    TIMESTAMPTZ
);
-- +goose StatementEnd

-- +goose StatementBegin
-- "Current version" probe: filter status='success', newest finished
-- first. Partial index keeps it tight (in_progress/failed rows don't
-- bloat it) and finished_at is always set on success rows.
CREATE INDEX idx_deployment_revisions_current
    ON deployment_revisions (environment_id, finished_at DESC)
    WHERE status = 'success';
-- +goose StatementEnd

-- +goose StatementBegin
-- History timeline per environment (all statuses), newest first.
CREATE INDEX idx_deployment_revisions_history
    ON deployment_revisions (environment_id, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- Finalise-on-result lookup, keyed by (job_run_id, attempt). UNIQUE
-- (partial) enforces the invariant the finalisation relies on: at
-- most ONE in_progress revision per (job_run, attempt), so a result
-- or a reaper-requeue finalises exactly the attempt that ran.
CREATE UNIQUE INDEX idx_deployment_revisions_inflight
    ON deployment_revisions (job_run_id, attempt)
    WHERE status = 'in_progress';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS deployment_revisions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS environments;
-- +goose StatementEnd
