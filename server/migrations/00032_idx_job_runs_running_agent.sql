-- +goose Up
-- +goose StatementBegin

-- Hot-path index for the register-fence's ListRunningJobsForAgent
-- query (`WHERE status='running' AND agent_id=$1`) and the snapshot-
-- validating UPDATEs in ReclaimJobForRetry / FailStaleJobAtMax.
--
-- The existing idx_job_runs_status is keyed on `status` (queued|running)
-- and the existing idx_job_runs_agent is keyed on `agent_id` (NOT NULL).
-- Neither alone is ideal for "all rows for THIS agent that are still
-- running" — Postgres would have to fetch one of the two and filter
-- the other in memory. On an agent that's been online for weeks with
-- thousands of historical terminal job_runs, that's a noticeable scan.
--
-- A partial composite index keyed on agent_id WHERE status='running'
-- (AND agent_id IS NOT NULL — implicit because we only index when
-- status=running, which by AssignJob's invariant requires agent_id to
-- be set) keeps the index tiny (at most `running_jobs_per_agent`
-- entries, usually < capacity) and makes the fence's lookup O(1) per
-- agent regardless of total job_runs cardinality.
CREATE INDEX IF NOT EXISTS idx_job_runs_running_agent
    ON job_runs(agent_id)
    WHERE status = 'running' AND agent_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_job_runs_running_agent;
-- +goose StatementEnd
