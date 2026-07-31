-- name: InsertEnvironmentFreeze :one
-- Freeze (project, name). ON CONFLICT DO NOTHING makes the toggle idempotent
-- AND preserves the ORIGINAL freeze: re-freezing an already-frozen env must
-- not reset frozen_at/frozen_by/reason (that would erase who stopped
-- production and why). Returns no row when it was already frozen, which is
-- what drives the "only audit a real state change" branch in the store.
INSERT INTO environment_freezes (project_id, name, frozen_by, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, name) DO NOTHING
RETURNING frozen_at;

-- name: DeleteEnvironmentFreeze :one
-- Unfreeze. The `project_id` predicate IS the scope check — no separate
-- EnvironmentBelongsToProject precheck is needed (and none would help: the
-- freeze can exist without an environments row). Returns no row when it
-- wasn't frozen, so unfreeze is idempotent and only a real thaw is audited.
DELETE FROM environment_freezes
WHERE project_id = $1 AND name = $2
RETURNING frozen_at, frozen_by, reason;

-- name: GetEnvironmentFreeze :one
-- Single-env freeze state, PK probe. ErrNoRows = not frozen.
SELECT frozen_at, frozen_by, reason
FROM environment_freezes
WHERE project_id = $1 AND name = $2;

-- name: EnvironmentIsFrozen :one
-- The authoritative admission re-check, run INSIDE the admitting tx while
-- holding ProjectEnvFreezeLockKey. Kept separate from GetEnvironmentFreeze so
-- the hot path scans one bool instead of three columns it would discard.
SELECT EXISTS (
    SELECT 1 FROM environment_freezes WHERE project_id = $1 AND name = $2
);

-- name: FrozenEnvironments :many
-- Batched freeze lookup for a set of env names: ONE query per scheduler tick
-- for the active stage's deploy envs (and per approval for a gate's governed
-- envs) instead of N per-job probes. Returns only the frozen ones, ORDER BY
-- name so callers can pick a DETERMINISTIC "winning" env — the run-level
-- queue_reason must not alternate between two frozen envs from tick to tick.
SELECT name
FROM environment_freezes
WHERE project_id = $1 AND name = ANY(@names::text[])
ORDER BY name;

-- name: ListRunsHeldByEnvironment :many
-- The runs a freeze is currently holding, so unfreeze can NOTIFY each one
-- awake instead of waiting up to a full scheduler tick.
--
-- EXACT equality on the composed reason, never LIKE: ValidEnvironmentName
-- allows `_`, which is a LIKE single-char wildcard, so
-- `queue_reason LIKE 'frozen-deploy:staging_eu'` would also match
-- `frozen-deploy:stagingXeu`. runs has no project_id (only pipeline_id), so
-- the project scope comes through pipelines.
SELECT r.id
FROM runs r
JOIN pipelines pl ON pl.id = r.pipeline_id
WHERE pl.project_id = $1
  AND r.queue_reason = 'frozen-deploy:' || @name::text
  AND r.status IN ('queued', 'running');
