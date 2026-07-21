-- name: UpsertDeployTarget :one
-- Register/update the deploy target for an environment (1:1). The admin API
-- EnsureEnvironment's the environment first, then upserts here.
INSERT INTO deploy_targets (
    environment_id, provider, cluster, application, namespace, sync_mode, created_by,
    rollout_aware, rollout_cluster, rollout_namespace, rollout_name
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (environment_id) DO UPDATE SET
    provider = EXCLUDED.provider,
    cluster = EXCLUDED.cluster,
    application = EXCLUDED.application,
    namespace = EXCLUDED.namespace,
    sync_mode = EXCLUDED.sync_mode,
    rollout_aware = EXCLUDED.rollout_aware,
    rollout_cluster = EXCLUDED.rollout_cluster,
    rollout_namespace = EXCLUDED.rollout_namespace,
    rollout_name = EXCLUDED.rollout_name,
    updated_at = NOW()
RETURNING id;

-- name: ResolveDeployTarget :one
-- Resolve `deploy: { to: <env> }` for a project: join the environment to its
-- target and return everything the provider + native takeover need (incl. the
-- environment id for the deployment_revision FK).
SELECT dt.provider, dt.cluster, dt.application, dt.namespace, dt.sync_mode,
       dt.rollout_aware, dt.rollout_cluster, dt.rollout_namespace, dt.rollout_name,
       e.project_id, e.id AS environment_id, e.name AS environment
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1 AND e.name = $2;

-- name: ListDeployTargetsForProject :many
SELECT dt.id, e.name AS environment, dt.provider, dt.cluster, dt.application,
       dt.namespace, dt.sync_mode,
       dt.rollout_aware, dt.rollout_cluster, dt.rollout_namespace, dt.rollout_name
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
-- Backs the cluster delete-guard: a cluster referenced by any target (as its
-- Application cluster OR its Rollout cluster) can't be deleted — also enforced by
-- both FKs' ON DELETE RESTRICT; this gives the friendly message.
SELECT COUNT(*) FROM deploy_targets WHERE cluster = $1 OR rollout_cluster = $1;
