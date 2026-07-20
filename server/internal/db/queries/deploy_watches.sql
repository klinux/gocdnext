-- name: CreateDeployWatch :one
-- Create the control-loop record for a freshly-created in_progress deployment
-- revision. Unclaimed (claim_* NULL) and sync_requested_at NULL: the row exists
-- BEFORE Sync fires so a crash between create and Sync is recoverable. The
-- WHERE EXISTS guard refuses to watch an already-terminal revision (a late or
-- duplicate create) — 0 rows → the store maps it to ErrRevisionNotInProgress.
-- The gate_* CONFIG columns are the target's governing_gate denormalized here at
-- creation (an immutable per-deploy snapshot; a mid-flight target edit must not change
-- an in-flight deploy's gate). gate_required NULL => this deploy is not gated. The
-- per-arm / decision / action gate columns stay NULL until the watcher arms a step.
INSERT INTO deploy_watches (
    deployment_revision_id, project_id, sync_mode, cluster, application,
    namespace, expected_revision, deadline_at,
    rollout_aware, rollout_cluster, rollout_namespace, rollout_name,
    gate_approvers, gate_approver_groups, gate_required, gate_description
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
       sqlc.narg(gate_approvers), sqlc.narg(gate_approver_groups),
       sqlc.narg(gate_required), sqlc.narg(gate_description)
WHERE EXISTS (
    SELECT 1 FROM deployment_revisions WHERE id = $1 AND status = 'in_progress'
)
RETURNING *;

-- name: StampRolloutObservation :execrows
-- Persist the observed Rollout snapshot onto the watch each tick, so the UI (which
-- reads the DB) can render canary progress. Fenced on claim_id — 0 rows means the
-- lease was lost. rollout_current_step is NULL when the controller hasn't reported it
-- (RolloutState.CurrentStepKnown=false). Clears rollout_error on a successful observe.
UPDATE deploy_watches
SET rollout_phase        = sqlc.narg(rollout_phase),
    rollout_message      = sqlc.narg(rollout_message),
    rollout_pause_reason = sqlc.narg(rollout_pause_reason),
    rollout_current_step = sqlc.narg(rollout_current_step),
    rollout_step_count   = sqlc.narg(rollout_step_count),
    rollout_aborted      = sqlc.narg(rollout_aborted),
    rollout_error        = sqlc.narg(rollout_error),
    rollout_observed_at  = NOW()
WHERE deployment_revision_id = sqlc.arg(deployment_revision_id)
  AND claim_id = sqlc.arg(claim_id);

-- name: ArmRolloutGate :execrows
-- Arm the approval gate for the current indefinite pause. Stamps a FRESH gate_id (the
-- anti-stale token), the armed-at time, the paused step, and the RESOLVED Rollout
-- identity PINNED for this gate — Promote/Abort act on these, never a re-discovery, so
-- `.status.resources[]` drift can't redirect the effect. All three identity columns are
-- stamped together (a complete pinned tuple; the watcher only arms with all three).
-- Fenced on claim_id; `gate_id IS NULL` makes a double-tick a no-op (never re-arms a
-- fresh token under an in-flight vote).
UPDATE deploy_watches
SET gate_id                = gen_random_uuid(),
    gate_armed_at          = NOW(),
    gate_paused_step       = sqlc.arg(gate_paused_step),
    gate_rollout_cluster   = sqlc.arg(gate_rollout_cluster),
    gate_rollout_namespace = sqlc.arg(gate_rollout_namespace),
    gate_rollout_name      = sqlc.arg(gate_rollout_name)
WHERE deployment_revision_id = sqlc.arg(deployment_revision_id)
  AND claim_id = sqlc.arg(claim_id)
  AND gate_id IS NULL;

-- name: MarkGateActioned :execrows
-- Record that the gated Promote/Abort was ISSUED for this decision (anti-retry only —
-- no deadline change; the terminal DECISION already resumed the deadline). Fenced on
-- claim_id; `gate_actioned_at IS NULL` makes a re-tick a no-op.
UPDATE deploy_watches
SET gate_actioned_at = NOW()
WHERE deployment_revision_id = sqlc.arg(deployment_revision_id)
  AND claim_id = sqlc.arg(claim_id)
  AND gate_actioned_at IS NULL;

-- name: ClearRolloutGateColumns :execrows
-- Disarm the current step's gate: null the per-arm / decision / action columns. The
-- CONFIG columns (gate_approvers/required/description) persist for the whole deploy, so
-- the next pause re-arms with the same policy. If cleared while STILL UNDECIDED (an
-- external promote before any vote), resume the deadline with the one-time shift
-- deadline_at += NOW() - gate_armed_at. The CASE reads the OLD row values (an UPDATE's
-- SET RHS all see the pre-update row), so it computes the shift before nulling. Guarded
-- so it's applied at most once — a DECIDED gate already had its deadline resumed by
-- DecideRolloutGate. Fenced on claim_id.
UPDATE deploy_watches
SET deadline_at = CASE
        WHEN gate_armed_at IS NOT NULL AND gate_decision IS NULL
        THEN deadline_at + (NOW() - gate_armed_at)
        ELSE deadline_at
    END,
    gate_id                = NULL,
    gate_armed_at          = NULL,
    gate_paused_step       = NULL,
    gate_rollout_cluster   = NULL,
    gate_rollout_namespace = NULL,
    gate_rollout_name      = NULL,
    gate_decision          = NULL,
    gate_decided_by        = NULL,
    gate_decided_at        = NULL,
    gate_actioned_at       = NULL
WHERE deployment_revision_id = sqlc.arg(deployment_revision_id)
  AND claim_id = sqlc.arg(claim_id);

-- name: DeleteDeployGateVotes :exec
-- Delete the current arm's approval votes so the next step's gate starts fresh. Votes
-- reuse job_run_approvals keyed on the deploy's job_run_id; each canary step is its own
-- round, so a step's votes must not carry into the next. The durable per-decision record
-- is the audit event emitted by DecideRolloutGate, not these transient rows.
DELETE FROM job_run_approvals
WHERE job_run_id = (
    SELECT job_run_id FROM deployment_revisions WHERE id = sqlc.arg(deployment_revision_id)
);

-- name: GetDeployWatch :one
SELECT * FROM deploy_watches WHERE deployment_revision_id = $1;

-- name: ClaimDeployWatches :many
-- Atomically claim a batch of claimable watches — never claimed, or lease-expired
-- (the prior watcher crashed) — assigning each a fresh fencing token. The join to
-- deployment_revisions filters to still-in_progress deploys: a backstop, since the
-- terminalizers (FinalizeDeploymentRevision / FinalizeDeployWatch) already delete the
-- watch — so a terminal-revision watch never reaches a watcher even if one lingered.
-- FOR UPDATE OF ... SKIP
-- LOCKED lets replicas claim disjoint batches without contending. Each row gets its
-- OWN claim_id (gen_random_uuid is volatile, evaluated per row).
UPDATE deploy_watches w
SET claim_id = gen_random_uuid(), claimed_by = sqlc.arg(claimed_by), claimed_at = NOW()
WHERE w.deployment_revision_id IN (
    SELECT dw.deployment_revision_id
    FROM deploy_watches dw
    JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
    WHERE dr.status = 'in_progress'
      AND (dw.claimed_at IS NULL
           OR dw.claimed_at < NOW() - make_interval(secs => sqlc.arg(lease_seconds)::int))
    ORDER BY dw.claimed_at NULLS FIRST, dw.created_at
    FOR UPDATE OF dw SKIP LOCKED
    LIMIT sqlc.arg(max_batch)
)
RETURNING *;

-- name: RenewDeployWatch :execrows
-- Heartbeat: extend the lease. Fenced on claim_id — 0 rows means the lease was
-- stolen (another replica reclaimed) and this watcher must drop the work.
UPDATE deploy_watches
SET claimed_at = NOW()
WHERE deployment_revision_id = $1 AND claim_id = $2;

-- name: MarkDeployWatchSyncRequested :execrows
-- Stamp the correlation anchor right after Sync fires. Fenced on claim_id.
UPDATE deploy_watches
SET sync_requested_at = NOW()
WHERE deployment_revision_id = $1 AND claim_id = $2;

-- name: StampDeployWatchSyncRequested :execrows
-- UNFENCED stamp of the correlation anchor at dispatch, before any watcher has
-- claimed the watch (so the fenced MarkDeployWatchSyncRequested can't be used yet).
-- Monotonic: `WHERE sync_requested_at IS NULL` — a later dispatch/retry never reopens
-- the anchor. 0 rows → already stamped or gone; the caller logs and continues.
UPDATE deploy_watches
SET sync_requested_at = NOW()
WHERE deployment_revision_id = $1 AND sync_requested_at IS NULL;

-- name: SetDeployWatchDegradedSince :execrows
-- Open the debounce window on the first Degraded tick (COALESCE keeps the earliest).
-- Fenced on claim_id.
UPDATE deploy_watches
SET degraded_since = COALESCE(degraded_since, NOW())
WHERE deployment_revision_id = $1 AND claim_id = $2;

-- name: ClearDeployWatchDegraded :execrows
-- Health recovered before the debounce elapsed: reset the anchor. Fenced on claim_id.
UPDATE deploy_watches
SET degraded_since = NULL
WHERE deployment_revision_id = $1 AND claim_id = $2;

-- name: DeleteDeployWatchClaimed :execrows
-- Fenced delete used by the atomic terminal tx: 0 rows means the lease was lost, so
-- the caller MUST NOT terminalize the deploy (fencing guarantee).
DELETE FROM deploy_watches
WHERE deployment_revision_id = $1 AND claim_id = $2;

-- name: CountActiveWatchesForCluster :one
-- Backs the cluster delete-guard: an in-flight watch referencing the cluster (as its
-- Application cluster OR its Rollout cluster) RESTRICTs it (both FKs); this gives the
-- friendly message before the DELETE fails.
SELECT COUNT(*) FROM deploy_watches WHERE cluster = $1 OR rollout_cluster = $1;

-- name: ListDeployWatchesForProject :many
-- In-flight native deploys for a project (one row per still-in_progress revision),
-- joined to the environment name + display version for the UI live-status endpoint.
-- Config fields (cluster/application/sync_mode) are returned here but sanitised by
-- role at the HTTP layer — viewers never see them.
SELECT dw.deployment_revision_id, e.name AS environment, dr.version,
       dw.expected_revision, dw.sync_mode,
       dw.cluster, dw.application, dw.watch_started_at, dw.sync_requested_at,
       dw.deadline_at, dw.degraded_since,
       dw.rollout_aware, dw.rollout_phase, dw.rollout_message, dw.rollout_pause_reason,
       dw.rollout_current_step, dw.rollout_step_count, dw.rollout_aborted,
       dw.rollout_error, dw.rollout_observed_at
FROM deploy_watches dw
JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
JOIN environments e ON e.id = dr.environment_id
WHERE dw.project_id = $1
ORDER BY e.name;
