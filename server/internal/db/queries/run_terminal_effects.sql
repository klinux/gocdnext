-- name: ClaimRunTerminalEffects :one
-- Claim generic terminal effects for a run. service_generation is returned with
-- the claim and later used by MarkRunTerminalEffectsDone so a stale worker cannot
-- mark a run's post-rerun terminalization done after RerunJob bumped generation.
WITH cur AS (
    SELECT runs.id,
           runs.status,
           runs.cause,
           runs.cancel_reason,
           runs.superseded_by,
           runs.service_generation,
           runs.terminal_effects_required AS required,
           runs.terminal_effects_claimed_at AS prev_claim,
           runs.terminal_effects_at AS effects_at
    FROM runs
    WHERE runs.id = $1
    FOR UPDATE
)
UPDATE runs r
SET terminal_effects_claimed_at = NOW()
FROM cur
WHERE r.id = cur.id
  AND cur.required = true
  AND cur.status NOT IN ('queued', 'running')
  AND cur.superseded_by IS NULL
  AND NOT (
      cur.cause = 'merge_group'
      AND COALESCE(cur.cancel_reason, '') LIKE 'github merge_group destroyed%'
  )
  AND cur.effects_at IS NULL
  AND (cur.prev_claim IS NULL
       OR cur.prev_claim < NOW() - sqlc.arg(lease)::INTERVAL)
RETURNING cur.status, cur.service_generation, (cur.prev_claim IS NULL)::boolean AS first_claim;

-- name: MarkRunTerminalEffectsDone :execrows
-- Complete the generic terminal-effects lease. The generation guard prevents an
-- old worker from completing a newer terminal event after RerunJob revived the
-- same run_id and bumped service_generation.
UPDATE runs
SET terminal_effects_at = NOW(),
    terminal_effects_required = false
WHERE id = $1
  AND service_generation = $2
  AND terminal_effects_required = true
  AND status NOT IN ('queued', 'running')
  AND superseded_by IS NULL
  AND NOT (
      cause = 'merge_group'
      AND COALESCE(cancel_reason, '') LIKE 'github merge_group destroyed%'
  )
  AND terminal_effects_claimed_at IS NOT NULL
  AND terminal_effects_at IS NULL;

-- name: ListPendingRunTerminalEffects :many
-- Replay terminal effects that missed their NOTIFY or whose claimer crashed.
SELECT id
FROM runs
WHERE terminal_effects_required = true
  AND status NOT IN ('queued', 'running')
  AND superseded_by IS NULL
  AND terminal_effects_at IS NULL
  AND (terminal_effects_claimed_at IS NULL
       OR terminal_effects_claimed_at < NOW() - sqlc.arg(lease)::INTERVAL)
  AND NOT (
      cause = 'merge_group'
      AND COALESCE(cancel_reason, '') LIKE 'github merge_group destroyed%'
  )
ORDER BY finished_at, id
LIMIT sqlc.arg(max_rows);

-- name: NotifyRunTerminalEffects :exec
-- Transactional wake-up for the generic terminal-effects worker. PostgreSQL only
-- delivers NOTIFY after the surrounding transaction commits.
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);
