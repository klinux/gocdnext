-- name: GetRunForAction :one
-- Thin row used by cancel/rerun handlers to check status + find the
-- pipeline + revisions without pulling the whole detail query.
SELECT id, pipeline_id, status, revisions
FROM runs
WHERE id = $1;

-- name: CancelActiveRun :one
-- Flips a run to 'canceled' only if it was still active. Idempotent:
-- a second call on a terminal run returns no rows so the handler
-- can answer 409. Returns the row id so the caller can tell the
-- update happened.
UPDATE runs
SET status = 'canceled', finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status IN ('queued', 'running')
RETURNING id;

-- name: GetLatestModificationForPipeline :one
-- Most recent modification across any material attached to a
-- pipeline. Powers "trigger latest" for manual runs. Ordered by
-- detected_at so the newest webhook delivery wins even when the
-- committer timestamp is older (rebases, fast-forwards of older
-- commits).
SELECT m.id, m.material_id, m.revision, m.branch
FROM modifications m
JOIN materials mat ON mat.id = m.material_id
WHERE mat.pipeline_id = $1
ORDER BY m.detected_at DESC
LIMIT 1;
