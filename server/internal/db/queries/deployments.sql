-- name: UpsertEnvironment :one
-- Lazy-create: the first dispatch of a job with deploy:{environment:X}
-- inserts the row; later dispatches just bump updated_at. Returns the
-- id so the dispatch path can attach a revision regardless of which
-- side of the conflict it hit.
INSERT INTO environments (project_id, name)
VALUES ($1, $2)
ON CONFLICT (project_id, name) DO UPDATE SET updated_at = NOW()
RETURNING id;

-- name: DeleteEnvironmentIfIdle :one
-- Hard-delete an environment scoped to its project, but ONLY when it has no
-- in-flight deploy — an in_progress deployment_revision or a live deploy_watch.
-- Blocking here (rather than cascading) stops the delete from orphaning a
-- running deploy whose revision + watch would vanish under it: the job_run would
-- stay running with no agent and the reaper would later re-see it as orphaned.
-- Once the env is confirmed idle, ON DELETE CASCADE still fans the delete out to
-- the FINALIZED history + any registered target (incl. a gated one), which is why
-- the API gates this to admin. Atomic: the idle check and the delete are one
-- statement, so a deploy that starts concurrently can't slip through a
-- check→delete gap. Returns three flags the caller maps to 204 / 409 / 404.
-- Environments are lazy, so a later deploy to the same name re-creates it empty.
WITH tgt AS (
    SELECT e.id FROM environments e WHERE e.project_id = $1 AND e.id = $2
), active AS (
    SELECT 1
    FROM deployment_revisions dr
    WHERE dr.environment_id = $2
      AND (dr.status = 'in_progress'
           OR EXISTS (SELECT 1 FROM deploy_watches dw
                      WHERE dw.deployment_revision_id = dr.id))
    LIMIT 1
), del AS (
    DELETE FROM environments e
    WHERE e.id IN (SELECT tgt.id FROM tgt) AND NOT EXISTS (SELECT 1 FROM active)
    RETURNING e.id
)
SELECT
    EXISTS (SELECT 1 FROM tgt)    AS existed,
    EXISTS (SELECT 1 FROM active) AS active,
    EXISTS (SELECT 1 FROM del)    AS deleted;

-- name: CreateDeploymentRevision :one
-- Recorded at dispatch with the resolved version, status in_progress,
-- tagged with the dispatch attempt so retries don't collide.
INSERT INTO deployment_revisions
    (environment_id, run_id, job_run_id, attempt, version, is_rollback, deployed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id;

-- name: DeleteDeploymentRevision :exec
-- Removes a revision created at dispatch when the dispatch then failed
-- to reach an agent (the frame never went out, so no deploy happened).
-- Scoped to in_progress so it can never erase a finalized audit row.
DELETE FROM deployment_revisions
WHERE id = $1 AND status = 'in_progress';

-- name: FinalizeDeploymentRevision :one
-- Called on the job's terminal result (or by the reaper for a dead
-- attempt): flips the in_progress revision of THIS (job_run, attempt)
-- to success/failed and stamps finished_at. Keyed on attempt so a
-- success on attempt 1 never finalises a stale attempt-0 row. Guarded
-- on status='in_progress' so a re-delivered result is a no-op. Returns
-- the count finalized (0 = the job had no deploy: block, or already final).
--
-- The `cleaned` data-modifying CTE atomically removes any deploy_watch for the
-- finalized revision: a native-provider deploy may be terminalized by THIS
-- job/reaper path (not only by the watcher's own FinalizeDeployWatch), and a watch
-- left behind would linger forever in the live-watch queue (never claimable — the
-- claim scan filters status='in_progress') and falsely block deleting its cluster.
WITH finalized AS (
    UPDATE deployment_revisions
    SET status = $3, finished_at = NOW()
    WHERE job_run_id = $1 AND attempt = $2 AND status = 'in_progress'
    RETURNING id
), cleaned AS (
    DELETE FROM deploy_watches
    WHERE deployment_revision_id IN (SELECT id FROM finalized)
)
SELECT count(*) FROM finalized;

-- name: FinalizeDeploymentRevisionByID :one
-- Terminalize a specific revision by id (the deploy watcher's convergence verdict)
-- and return its job link so the SAME tx can complete the server-managed deploy
-- job_run (ADR-0001, Model A). Guarded on status='in_progress' so a re-delivered
-- finalize is a no-op (ErrNoRows). The watcher's FinalizeDeployWatch deletes the
-- deploy_watch (fenced) in the same tx before calling this.
UPDATE deployment_revisions
SET status = $2, finished_at = NOW()
WHERE id = $1 AND status = 'in_progress'
RETURNING job_run_id, attempt;

-- name: ListEnvironmentsByProject :many
SELECT id, project_id, name, description, created_at, updated_at
FROM environments
WHERE project_id = $1
ORDER BY name;

-- name: ListEnvironmentsWithCurrentByProject :many
-- One row per environment with its CURRENT deployment (newest
-- successful revision) attached via LEFT JOIN LATERAL — environments
-- with nothing deployed yet still appear, with NULL current_* columns.
-- The LATERAL probe lands straight on idx_deployment_revisions_current
-- (one index hit per environment), so this is the single query behind
-- the Environments tab — no N+1.
-- current_id is the has-a-deployment discriminator (NULL = nothing
-- deployed yet). version/is_rollback/deployed_by are COALESCEd because
-- the LEFT JOIN yields NULL for an undeployed env and their base
-- columns are NOT NULL (sqlc would type them non-nullable and the
-- scan would fail on NULL); the caller gates on current_id.Valid.
SELECT e.id, e.name, e.description, e.created_at, e.updated_at,
       c.id                           AS current_id,
       c.run_id                       AS current_run_id,
       COALESCE(c.attempt, 0)         AS current_attempt,
       COALESCE(c.version, '')        AS current_version,
       COALESCE(c.is_rollback, FALSE) AS current_is_rollback,
       COALESCE(c.deployed_by, '')    AS current_deployed_by,
       c.created_at                   AS current_created_at,
       c.finished_at                  AS current_finished_at
FROM environments e
LEFT JOIN LATERAL (
    SELECT id, run_id, attempt, version, is_rollback, deployed_by, created_at, finished_at
    FROM deployment_revisions
    WHERE environment_id = e.id AND status = 'success'
    ORDER BY finished_at DESC
    LIMIT 1
) c ON TRUE
WHERE e.project_id = $1
ORDER BY e.name;

-- name: GetDeploymentRevision :one
SELECT id, environment_id, run_id, job_run_id, attempt, version, status,
       is_rollback, deployed_by, created_at, finished_at
FROM deployment_revisions
WHERE id = $1;

-- name: EnvironmentBelongsToProject :one
-- Scope guard: confirm an environment id is owned by a project before
-- serving its deployments through that project's URL.
SELECT EXISTS (
    SELECT 1 FROM environments WHERE id = $2 AND project_id = $1
);

-- name: UpdateEnvironmentDescription :exec
UPDATE environments
SET description = $3, updated_at = NOW()
WHERE project_id = $1 AND id = $2;

-- name: CurrentDeploymentByEnvironment :one
-- "What's deployed now": newest successful revision. Served straight
-- off idx_deployment_revisions_current.
SELECT id, environment_id, run_id, job_run_id, attempt, version, status,
       is_rollback, deployed_by, created_at, finished_at
FROM deployment_revisions
WHERE environment_id = $1 AND status = 'success'
ORDER BY finished_at DESC
LIMIT 1;

-- name: ListDeploymentHistory :many
-- Timeline for one environment, all statuses, newest first.
SELECT id, environment_id, run_id, job_run_id, attempt, version, status,
       is_rollback, deployed_by, created_at, finished_at
FROM deployment_revisions
WHERE environment_id = $1
ORDER BY created_at DESC
LIMIT $2;
