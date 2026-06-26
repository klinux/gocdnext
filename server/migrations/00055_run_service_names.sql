-- +goose Up
-- +goose StatementBegin

-- runs.service_names snapshots the NAMES of the services the run's
-- pipeline declared at the moment the run was CREATED. It mirrors
-- runs.has_services (migration 00036): has_services answers "did this
-- run declare any services?", service_names answers "which ones?".
--
-- Same rationale as has_services — the pipelines list / project page
-- can render the declared service names without re-reading the CURRENT
-- pipeline definition, which can drift mid-run via ApplyProject. The
-- runtime stamps this column from the SAME `domain.Pipeline` it uses to
-- materialise stages + jobs and to compute has_services
-- (store.insertRunSkeleton), so the snapshot can't disagree with the
-- rest of the run under concurrent ApplyProject + READ COMMITTED.
--
-- DEFAULT '{}' (empty array, never NULL) so a run with no services
-- reads back as an empty list rather than a NULL the Go/JSON layers
-- would have to special-case.
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS service_names TEXT[] NOT NULL DEFAULT '{}';

-- Backfill existing runs: best-effort, derive the names from the
-- current pipeline definition. Same caveat as the has_services backfill
-- (migration 00036) — wrong for runs whose pipeline was reapplied
-- between create and now, but for greenfield this is a near-no-op.
--
-- Scoped to `has_services = true`: only runs that actually declared
-- services can need names, so this avoids rewriting (WAL + bloat) every
-- other row to the same '{}' the DEFAULT already gave them. The CASE
-- guards jsonb_array_elements against a `Services` that drifted to a
-- non-array (scalar/object) in the current definition — it would error
-- otherwise.
UPDATE runs r
SET service_names = COALESCE(
    (
        SELECT array_agg(svc->>'Name' ORDER BY ord)
        FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(p.definition->'Services') = 'array'
                 THEN p.definition->'Services'
                 ELSE '[]'::jsonb END
        ) WITH ORDINALITY AS t(svc, ord)
        WHERE svc->>'Name' IS NOT NULL
    ),
    '{}'
)
FROM pipelines p
WHERE r.pipeline_id = p.id
  AND r.has_services = true;

COMMENT ON COLUMN runs.service_names IS
    'Snapshot of pipeline.definition->Services names at run create. Mirrors has_services; immutable post-insert.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs
    DROP COLUMN IF EXISTS service_names;
-- +goose StatementEnd
