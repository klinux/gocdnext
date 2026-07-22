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

-- name: LockDeployTargetForDelete :one
-- Locks the target row FOR UPDATE and reads its gate, so the non-admin delete's
-- ungated-check happens on the LOCKED row. A concurrent admin gate-add blocks on this
-- lock (or is seen post-commit), closing the TOCTOU: the delete decision can't observe
-- a stale ungated snapshot and then remove a row that became gated. ErrNoRows => the
-- target is absent (404). Runs in the same tx as DeleteDeployTargetByID.
SELECT dt.id, dt.governing_gate
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1 AND e.name = $2
FOR UPDATE OF dt;

-- name: DeleteDeployTargetByID :execrows
-- Deletes a specific target row (used after LockDeployTargetForDelete decided the
-- locked row is ungated). Same tx / lock as the lock-read.
DELETE FROM deploy_targets WHERE id = $1;

-- name: CountDeployTargetsForCluster :one
-- Backs the cluster delete-guard: a cluster referenced by any target (as its
-- Application cluster OR its Rollout cluster) can't be deleted — also enforced by
-- both FKs' ON DELETE RESTRICT; this gives the friendly message.
SELECT COUNT(*) FROM deploy_targets WHERE cluster = $1 OR rollout_cluster = $1;

-- name: LockDeployTargetForDeploy :one
-- Locks the target row FOR UPDATE and reads EVERYTHING the takeover needs, so a
-- DECLARED deploy decides and writes against one consistent snapshot. Mirrors
-- LockDeployTargetForDelete's pattern, and exists for the same reason: comparing the
-- declared base fields BEFORE this tx would be check-then-act across a transaction
-- boundary — a governing_gate added in that gap would still yield an UNGATED deploy,
-- because StartNativeDeploy never re-reads deploy_targets.
--
-- The caller classifies in Go (gate present => terminal, base mismatch/absent => retry);
-- a single conditional WHERE returning "0 rows" could not tell those apart, and the two
-- have opposite outcomes. It also builds the revision/watch from THESE values — an
-- environment deleted-and-recreated, or rollout routing changed since the reconcile,
-- must not be written from a pre-lock snapshot.
SELECT dt.id, dt.environment_id, dt.cluster, dt.application, dt.namespace, dt.sync_mode,
       dt.rollout_aware, dt.rollout_cluster, dt.rollout_namespace, dt.rollout_name,
       dt.governing_gate
FROM deploy_targets dt
JOIN environments e ON e.id = dt.environment_id
WHERE e.project_id = $1 AND e.name = $2
FOR UPDATE OF dt;
