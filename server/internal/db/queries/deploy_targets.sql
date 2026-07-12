-- name: UpsertDeployTarget :one
-- Register/update the deploy target for an environment (1:1). The admin API
-- EnsureEnvironment's the environment first, then upserts here.
INSERT INTO deploy_targets (environment_id, provider, cluster, application, namespace, sync_mode, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (environment_id) DO UPDATE SET
    provider = EXCLUDED.provider,
    cluster = EXCLUDED.cluster,
    application = EXCLUDED.application,
    namespace = EXCLUDED.namespace,
    sync_mode = EXCLUDED.sync_mode,
    updated_at = NOW()
RETURNING id;

-- name: ResolveDeployTarget :one
-- Resolve `deploy: { to: <env> }` for a project: join the environment to its
-- target and return everything the provider needs.
SELECT dt.provider, dt.cluster, dt.application, dt.namespace, dt.sync_mode,
       e.project_id, e.name AS environment
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1 AND e.name = $2;

-- name: ListDeployTargetsForProject :many
SELECT dt.id, e.name AS environment, dt.provider, dt.cluster, dt.application,
       dt.namespace, dt.sync_mode
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1
ORDER BY e.name;

-- name: DeleteDeployTargetByEnvironment :execrows
DELETE FROM deploy_targets
WHERE environment_id = (
    SELECT id FROM environments WHERE project_id = $1 AND name = $2
);

-- name: CountDeployTargetsForCluster :one
-- Backs the cluster delete-guard: a cluster referenced by any target can't be
-- deleted (also enforced by the FK's ON DELETE RESTRICT, this gives the message).
SELECT COUNT(*) FROM deploy_targets WHERE cluster = $1;
