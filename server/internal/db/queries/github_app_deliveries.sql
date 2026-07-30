-- name: ClaimGithubAppDelivery :execrows
-- Atomically claim a delivery id. Returns rows-affected: 1 = we claimed it
-- (proceed), 0 = a row already exists (duplicate/concurrent — the caller's tx
-- rolls back). Run inside the run-creation transaction so the claim + the run
-- commit together (exactly-once, crash-safe). The PK is the mutual-exclusion
-- point: a concurrent claim blocks here until this tx commits or rolls back.
INSERT INTO github_app_deliveries (delivery_id, event)
VALUES ($1, $2)
ON CONFLICT (delivery_id) DO NOTHING;

-- name: SetGithubAppDeliveryRun :exec
-- Link the run this delivery produced (same tx as the claim + run insert).
UPDATE github_app_deliveries SET run_id = $2 WHERE delivery_id = $1;

-- name: DeleteOldGithubAppDeliveries :execrows
-- Retention sweep: prune ledger rows past the cutoff ($1). Keys are opaque
-- delivery ids, safe to drop once well past their redelivery window.
DELETE FROM github_app_deliveries WHERE created_at < $1;
