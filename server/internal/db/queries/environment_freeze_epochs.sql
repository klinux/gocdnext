-- name: UpsertFreezeEpoch :exec
-- Record (or refresh) the instant a freeze was lifted on (project, env), the
-- FLOOR the approval expirer adds to a gate's expiry clock (#208). clock_timestamp()
-- (real wall time), NOT now()/transaction-start, because the expirer compares it
-- against clock_timestamp() taken after acquiring its locks.
--
-- Called ONLY on a real unfreeze (a freeze row was actually deleted), inside
-- UnfreezeEnvironment's tx under the per-(project, env) freeze advisory lock, so
-- an expiry racing the unfreeze reads a floor consistent with the freeze state it
-- checks under the same lock. An idempotent unfreeze that deleted nothing does
-- NOT call this, so it never renews a window nobody was waiting on.
INSERT INTO environment_freeze_epochs (project_id, environment, last_unfrozen_at)
VALUES ($1, $2, clock_timestamp())
ON CONFLICT (project_id, environment)
DO UPDATE SET last_unfrozen_at = clock_timestamp();

-- name: MaxLastUnfrozenAt :one
-- The most recent unfreeze instant across a gate's GovernedFreezeEnvs — the
-- floor the expirer folds into effective_start = max(awaiting_since, this). MAX
-- over no matching rows (envs that were never frozen) is NULL, which the caller
-- reads as "no floor" and falls back to awaiting_since — identical to pre-#208
-- behaviour for the un-frozen case. Read under the freeze advisory lock so the
-- floor is serialized against a concurrent unfreeze upsert.
SELECT MAX(last_unfrozen_at)::timestamptz AS max_unfrozen
FROM environment_freeze_epochs
WHERE project_id = $1 AND environment = ANY(@environments::text[]);
