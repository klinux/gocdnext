-- name: ListCronMaterials :many
-- Loads every cron material in the system alongside its pipeline
-- / project id and its last-fired timestamp (NULL when never
-- fired). The ticker walks this list on every tick; N is bounded
-- by `pipelines × cron_materials_per_pipeline`, typically a few
-- dozen at most, so scanning in-process is fine.
SELECT m.id, m.pipeline_id, pl.project_id, m.config,
       cs.last_fired_at
FROM materials m
JOIN pipelines pl ON pl.id = m.pipeline_id
LEFT JOIN cron_state cs ON cs.material_id = m.id
WHERE m.type = 'cron'
ORDER BY m.id;

-- name: UpsertCronFired :exec
-- Records a fire time for a cron material. The ticker calls this
-- right after dispatching a run so a crashed server doesn't re-
-- fire the same tick when it comes back.
INSERT INTO cron_state (material_id, last_fired_at)
VALUES ($1, $2)
ON CONFLICT (material_id) DO UPDATE SET
    last_fired_at = EXCLUDED.last_fired_at,
    updated_at    = NOW();
