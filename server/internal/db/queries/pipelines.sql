-- name: UpsertPipeline :one
INSERT INTO pipelines (project_id, name, definition, config_repo, config_path)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, name) DO UPDATE SET
    definition = EXCLUDED.definition,
    definition_version = CASE
        WHEN pipelines.definition = EXCLUDED.definition THEN pipelines.definition_version
        ELSE pipelines.definition_version + 1
    END,
    config_repo = EXCLUDED.config_repo,
    config_path = EXCLUDED.config_path,
    updated_at = CASE
        WHEN pipelines.definition = EXCLUDED.definition
             AND pipelines.config_repo IS NOT DISTINCT FROM EXCLUDED.config_repo
             AND pipelines.config_path = EXCLUDED.config_path
        THEN pipelines.updated_at
        ELSE NOW()
    END
RETURNING id, project_id, name, definition_version, config_path, created_at, updated_at, (xmax = 0) AS created;

-- name: ListPipelinesByProject :many
SELECT id, name, definition_version
FROM pipelines
WHERE project_id = $1
ORDER BY name;

-- name: DeletePipeline :exec
DELETE FROM pipelines WHERE id = $1;
