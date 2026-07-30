-- name: ClaimGithubAppDelivery :execrows
-- Atomically claim a delivery id for processing. Returns rows-affected: 1 = we
-- claimed it (proceed), 0 = a row already exists (redelivery or concurrent
-- handler — caller inspects it via GetGithubAppDelivery). The PK makes this the
-- mutual-exclusion point: concurrent redeliveries can't both claim.
INSERT INTO github_app_deliveries (delivery_id, event)
VALUES ($1, $2)
ON CONFLICT (delivery_id) DO NOTHING;

-- name: GetGithubAppDelivery :one
SELECT delivery_id, event, status, run_id, created_at, updated_at
FROM github_app_deliveries
WHERE delivery_id = $1;

-- name: ReclaimStaleGithubAppDelivery :execrows
-- Re-claim a crashed attempt: a row stuck in 'processing' older than the stale
-- cutoff ($2) is taken over (updated_at bumped). Rows-affected 1 = we won the
-- re-claim (proceed), 0 = someone else already did or it's no longer stale.
UPDATE github_app_deliveries
SET updated_at = NOW()
WHERE delivery_id = $1
  AND status = 'processing'
  AND updated_at < $2;

-- name: MarkGithubAppDeliveryDone :exec
-- Mark a claimed delivery done + link the run it produced. Redeliveries then
-- see status='done' and short-circuit.
UPDATE github_app_deliveries
SET status = 'done', run_id = $2, updated_at = NOW()
WHERE delivery_id = $1;

-- name: ReleaseGithubAppDelivery :exec
-- Release a claim after a failed rerun so GitHub's automatic redelivery can
-- retry cleanly (a user re-click is a new delivery id anyway).
DELETE FROM github_app_deliveries
WHERE delivery_id = $1 AND status = 'processing';

-- name: DeleteOldGithubAppDeliveries :execrows
-- Retention sweep: prune completed ledger rows past the cutoff ($1). Keys are
-- opaque delivery ids, safe to drop once well past their redelivery window.
DELETE FROM github_app_deliveries
WHERE status = 'done' AND updated_at < $1;
