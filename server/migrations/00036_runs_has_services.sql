-- +goose Up
-- +goose StatementBegin

-- runs.has_services snapshots whether the run's pipeline declared a
-- non-empty `services:` block at the moment the run was CREATED.
-- Drives the CleanupRunServices broadcast in the run-terminal
-- cascade — without this snapshot, the cleanup gate would read the
-- CURRENT pipeline definition, which can drift mid-run via
-- ApplyProject. Two failure modes the snapshot fixes:
--
--   1. Run created with services → pipeline reapplied removing
--      services → run reaches terminal → live-definition check
--      returns false → cleanup skipped → service pods leak.
--   2. Run created WITHOUT services → pipeline reapplied adding
--      services → run reaches terminal → live-definition check
--      returns true → server dispatches cleanup → agents do a
--      pointless k8s List that returns 0 pods.
--
-- The runtime stamps this column from the SAME `domain.Pipeline`
-- it uses to materialise stages + jobs (store.insertRunSkeleton),
-- so the snapshot also can't disagree with the rest of the run
-- under concurrent ApplyProject + READ COMMITTED.
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS has_services BOOLEAN NOT NULL DEFAULT false;

-- Backfill existing runs: best-effort, derive from current pipeline
-- definition. Wrong for runs whose pipeline was reapplied between
-- create and now, but that case can only set the column to a value
-- that matches what's TRUE today — operator can manually correct
-- via SQL if a stale terminal run leaks pods. For greenfield this
-- backfill is a near-no-op (few rows pre-upgrade).
UPDATE runs r
SET has_services = COALESCE(
    p.definition->'Services' IS NOT NULL
    AND jsonb_typeof(p.definition->'Services') = 'array'
    AND jsonb_array_length(p.definition->'Services') > 0,
    false
)
FROM pipelines p
WHERE r.pipeline_id = p.id
  AND r.has_services = false;

COMMENT ON COLUMN runs.has_services IS
    'Snapshot of pipeline.definition->Services non-emptiness at run create. Drives CleanupRunServices dispatch; immutable post-insert.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs
    DROP COLUMN IF EXISTS has_services;
-- +goose StatementEnd
