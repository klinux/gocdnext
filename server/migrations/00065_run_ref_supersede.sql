-- +goose Up
-- +goose StatementBegin

-- Latest-wins supersede for approval-gated pipelines (issue #97).
--
-- runs.ref denormalises the triggering branch into the run at CREATE time
-- (mirrors service_names / has_services — the runtime stamps it from the same
-- trigger context in store.insertRunSkeleton, so it can't drift under a
-- concurrent ApplyProject). It is the supersede LANE key: latest-wins is scoped
-- to (pipeline_id, ref) for `supersede: branch`, or (pipeline_id) for
-- `supersede: pipeline`. Empty '' = one lane per pipeline (tags / manual-no-branch).
--
-- superseded_by points at the newer run that made this one moot; the run's
-- status stays 'canceled' (no new terminal status — see the design). cancel_reason
-- is shared with manual cancel and cites the superseding run's counter (#N), never
-- a value.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS ref           TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS superseded_by UUID REFERENCES runs(id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cancel_reason TEXT;

-- Best-effort backfill so PENDING pre-feature runs land in the right branch lane
-- when an operator first enables supersede (avoids an "old pending" gap). Derive
-- the branch from any material's entry in the revisions JSONB. CONSERVATIVE: the
-- jsonb_typeof guard tolerates non-object/old revisions; a tag / cron / manual run
-- with no branch simply keeps ref='' (a wider lane beats a fragile migration).
-- Scoped to non-terminal runs — finished runs never re-enter a lane.
UPDATE runs r
SET ref = COALESCE((
    SELECT rev.value->>'branch'
    FROM jsonb_each(
        CASE WHEN jsonb_typeof(r.revisions) = 'object' THEN r.revisions ELSE '{}'::jsonb END
    ) AS rev
    WHERE COALESCE(rev.value->>'branch', '') <> ''
    LIMIT 1
), '')
WHERE r.status IN ('queued', 'running');

-- run_gate_pass records, per (run, CONCRETE deploy environment), that the run has
-- cleared the approval gate(s) governing that environment. The dispatch backstop
-- reads it: a deploy for env E is refused when a NEWER still-active run in the lane
-- already passed the gate for E. Concrete env only (no '' wildcard) so the advisory
-- lock key is identical between the approve-time marker write and the dispatch
-- guard (the TOCTOU serialization). A gate governing no deploy job writes no row.
CREATE TABLE IF NOT EXISTS run_gate_pass (
    run_id      UUID   NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pipeline_id UUID   NOT NULL,
    ref         TEXT   NOT NULL,
    counter     BIGINT NOT NULL,
    environment TEXT   NOT NULL,
    passed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (run_id, environment),
    -- concrete non-empty env name only; defends against drift / manual DB poke
    -- (the parser already normalises + validates the environment name).
    CONSTRAINT run_gate_pass_env_chk
        CHECK (environment ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$')
);
-- backstop lookup per lane mode
CREATE INDEX IF NOT EXISTS run_gate_pass_branch_idx   ON run_gate_pass (pipeline_id, ref, environment, counter);
CREATE INDEX IF NOT EXISTS run_gate_pass_pipeline_idx ON run_gate_pass (pipeline_id, environment, counter);

-- victim lane lookup (pending runs), per lane mode
CREATE INDEX IF NOT EXISTS runs_lane_pending_branch_idx   ON runs (pipeline_id, ref, counter)
    WHERE status IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS runs_lane_pending_pipeline_idx ON runs (pipeline_id, counter)
    WHERE status IN ('queued', 'running');
-- the victim/marker predicates need "run has a pending gate" — index it small
CREATE INDEX IF NOT EXISTS job_runs_gate_pending_idx ON job_runs (run_id)
    WHERE approval_gate = true AND status = 'awaiting_approval';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS job_runs_gate_pending_idx;
DROP INDEX IF EXISTS runs_lane_pending_pipeline_idx;
DROP INDEX IF EXISTS runs_lane_pending_branch_idx;
DROP TABLE IF EXISTS run_gate_pass;
ALTER TABLE runs DROP COLUMN IF EXISTS cancel_reason;
ALTER TABLE runs DROP COLUMN IF EXISTS superseded_by;
ALTER TABLE runs DROP COLUMN IF EXISTS ref;
-- +goose StatementEnd
