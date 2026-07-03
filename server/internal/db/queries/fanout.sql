-- name: FindDownstreamUpstreamMaterials :many
-- For a completed stage on an upstream pipeline, find every "upstream"
-- material in OTHER pipelines of the same project that points at it. The
-- status filter respects `materials.config.status` (default 'success') so a
-- user can gate downstream on specific outcomes.
SELECT m.id AS material_id, m.pipeline_id AS downstream_pipeline_id, m.config
FROM materials m
JOIN pipelines down ON down.id = m.pipeline_id
JOIN pipelines up ON up.id = @upstream_pipeline_id::uuid
                 AND up.project_id = down.project_id
WHERE m.type = 'upstream'
  AND m.config->>'pipeline' = up.name
  AND m.config->>'stage' = @stage_name::text
  AND COALESCE(m.config->>'status', 'success') = 'success'
ORDER BY down.name;

-- name: GetStageSummary :one
-- Everything the fanout trigger needs to identify this stage's position
-- (pipeline + run + counter + revisions) without multiple round-trips.
SELECT s.id AS stage_run_id, s.name AS stage_name,
       r.id AS run_id, r.pipeline_id, r.counter, r.revisions,
       p.name AS pipeline_name
FROM stage_runs s
JOIN runs r ON r.id = s.run_id
JOIN pipelines p ON p.id = r.pipeline_id
WHERE s.id = $1
LIMIT 1;

-- name: FindRunByUpstream :one
-- Idempotency check for fanout: if we already created a downstream run for
-- this (pipeline, upstream_run_id) pair, skip.
SELECT id, counter
FROM runs
WHERE pipeline_id = @pipeline_id::uuid
  AND cause = 'upstream'
  AND (cause_detail->>'upstream_run_id')::uuid = @upstream_run_id::uuid
LIMIT 1;
