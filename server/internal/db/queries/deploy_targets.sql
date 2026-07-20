-- name: UpsertDeployTarget :one
-- Register/update the deploy target for an environment (1:1). The admin API
-- EnsureEnvironment's the environment first, then upserts here. governing_gate is
-- the JSONB gate config (NULL => no gate); the caller's separation-of-duties check
-- (admin-only to change a gate/routing on a gated target) runs BEFORE this write.
INSERT INTO deploy_targets (
    environment_id, provider, cluster, application, namespace, sync_mode, created_by,
    rollout_aware, rollout_cluster, rollout_namespace, rollout_name, governing_gate
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
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
    governing_gate = EXCLUDED.governing_gate,
    updated_at = NOW()
RETURNING id;

-- name: UpsertDeployTargetGuarded :one
-- The NON-ADMIN upsert: same as UpsertDeployTarget, but the ON CONFLICT UPDATE applies
-- ONLY if the row's gate + routing still equal what the caller's separation-of-duties
-- check authorized against (the expected_* params, captured at the SoD read). This is
-- the optimistic-concurrency backstop for the TOCTOU between that read and this write:
-- if an admin changed the gate/routing in between, the guard fails, 0 rows return, and
-- the store maps that to ErrDeployTargetConflict (409) — a stale non-admin write can
-- never clobber a concurrent admin gate change. The INSERT branch (a fresh create) is
-- unguarded (no existing gate to protect); the registrar's SoD check independently
-- rejects a non-admin CREATING a gate.
INSERT INTO deploy_targets (
    environment_id, provider, cluster, application, namespace, sync_mode, created_by,
    rollout_aware, rollout_cluster, rollout_namespace, rollout_name, governing_gate
)
VALUES (
    sqlc.arg(environment_id), sqlc.arg(provider), sqlc.arg(cluster), sqlc.arg(application),
    sqlc.arg(namespace), sqlc.arg(sync_mode), sqlc.arg(created_by),
    sqlc.arg(rollout_aware), sqlc.narg(rollout_cluster), sqlc.narg(rollout_namespace),
    sqlc.narg(rollout_name), sqlc.narg(governing_gate)
)
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
    governing_gate = EXCLUDED.governing_gate,
    updated_at = NOW()
WHERE deploy_targets.governing_gate IS NOT DISTINCT FROM sqlc.narg(expected_gate)::jsonb
  AND deploy_targets.rollout_aware = sqlc.arg(expected_rollout_aware)
  AND deploy_targets.rollout_cluster IS NOT DISTINCT FROM sqlc.narg(expected_rollout_cluster)
  AND deploy_targets.rollout_namespace IS NOT DISTINCT FROM sqlc.narg(expected_rollout_namespace)
  AND deploy_targets.rollout_name IS NOT DISTINCT FROM sqlc.narg(expected_rollout_name)
RETURNING id;

-- name: ResolveDeployTarget :one
-- Resolve `deploy: { to: <env> }` for a project: join the environment to its
-- target and return everything the provider + native takeover need (incl. the
-- environment id for the deployment_revision FK).
SELECT dt.provider, dt.cluster, dt.application, dt.namespace, dt.sync_mode,
       dt.rollout_aware, dt.rollout_cluster, dt.rollout_namespace, dt.rollout_name,
       dt.governing_gate,
       e.project_id, e.id AS environment_id, e.name AS environment
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1 AND e.name = $2;

-- name: ListDeployTargetsForProject :many
SELECT dt.id, e.name AS environment, dt.provider, dt.cluster, dt.application,
       dt.namespace, dt.sync_mode,
       dt.rollout_aware, dt.rollout_cluster, dt.rollout_namespace, dt.rollout_name,
       dt.governing_gate
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1
ORDER BY e.name;

-- name: DeleteDeployTargetByEnvironment :execrows
DELETE FROM deploy_targets
WHERE environment_id = (
    SELECT id FROM environments WHERE project_id = $1 AND name = $2
);

-- name: DeleteUngatedDeployTargetByEnvironment :one
-- The NON-ADMIN delete: removes the target only if it is UNGATED, and reports the
-- precise outcome in one atomic statement (one snapshot — no TOCTOU) so the handler can
-- pick the right status without a separate racy read:
--   'deleted' — was ungated, removed;
--   'gated'   — exists but has a gate (deleting a gated target is admin-only) -> 403;
--   'absent'  — no such target -> 404.
WITH tgt AS (
    SELECT id, governing_gate FROM deploy_targets
    WHERE environment_id = (SELECT id FROM environments WHERE project_id = $1 AND name = $2)
),
del AS (
    DELETE FROM deploy_targets
    WHERE id = (SELECT id FROM tgt WHERE governing_gate IS NULL)
    RETURNING id
)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM del) THEN 'deleted'
    WHEN EXISTS (SELECT 1 FROM tgt WHERE governing_gate IS NOT NULL) THEN 'gated'
    ELSE 'absent'
END AS outcome;

-- name: CountDeployTargetsForCluster :one
-- Backs the cluster delete-guard: a cluster referenced by any target (as its
-- Application cluster OR its Rollout cluster) can't be deleted — also enforced by
-- both FKs' ON DELETE RESTRICT; this gives the friendly message.
SELECT COUNT(*) FROM deploy_targets WHERE cluster = $1 OR rollout_cluster = $1;
