-- name: ListProjectCronsByProject :many
-- UI: list schedules bound to one project, newest first so
-- recently-added shows at the top.
SELECT id, project_id, name, expression, pipeline_ids, enabled,
       last_fired_at, created_by, created_at, updated_at
FROM project_crons
WHERE project_id = $1
ORDER BY created_at DESC, id ASC;

-- name: ListEnabledProjectCrons :many
-- Ticker path: every enabled schedule in the system + its
-- last-fired bookkeeping. N is small (projects × a handful of
-- schedules each), so full-scan-in-memory is fine.
SELECT id, project_id, name, expression, pipeline_ids,
       last_fired_at
FROM project_crons
WHERE enabled = TRUE
ORDER BY id;

-- name: GetProjectCron :one
SELECT id, project_id, name, expression, pipeline_ids, enabled,
       last_fired_at, created_by, created_at, updated_at
FROM project_crons
WHERE id = $1
LIMIT 1;

-- name: InsertProjectCron :one
INSERT INTO project_crons
    (project_id, name, expression, pipeline_ids, enabled, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, project_id, name, expression, pipeline_ids, enabled,
          last_fired_at, created_by, created_at, updated_at;

-- name: UpdateProjectCron :exec
-- Full-record update: UI saves the entire edited row so absent
-- fields mean "cleared". Intentionally does NOT touch
-- last_fired_at — that's bookkeeping the ticker owns.
UPDATE project_crons
SET name = $2,
    expression = $3,
    pipeline_ids = $4,
    enabled = $5,
    updated_at = NOW()
WHERE id = $1;

-- name: DeleteProjectCron :exec
DELETE FROM project_crons WHERE id = $1;

-- name: MarkProjectCronFired :exec
-- Called by the ticker after a successful evaluation+fire cycle.
-- Same-second fires are gated via the cron expression parser
-- reading last_fired_at — the store update closes that loop.
UPDATE project_crons
SET last_fired_at = $2, updated_at = NOW()
WHERE id = $1;
