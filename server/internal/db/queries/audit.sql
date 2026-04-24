-- name: InsertAuditEvent :one
-- Write-only hot path — every RBAC'd mutation fires one of these.
-- at is stamped by the DB so clock skew on multi-replica setups
-- doesn't re-order events inside a single tick.
INSERT INTO audit_events (actor_id, actor_email, action, target_type, target_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, actor_id, actor_email, action, target_type, target_id, metadata, at;

-- name: ListAuditEvents :many
-- Reverse-chrono listing with optional filters + offset
-- pagination. Empty string on action / target_type / actor
-- disables that filter; nil actor_id_filter disables actor
-- filtering (applied separately because typing empty UUID as
-- "no filter" leaks).
SELECT id, actor_id, actor_email, action, target_type, target_id, metadata, at
FROM audit_events
WHERE ($1::TEXT = '' OR action = $1)
  AND ($2::TEXT = '' OR target_type = $2)
  AND ($3::TEXT = '' OR actor_email ILIKE '%' || $3 || '%')
  AND ($4::UUID IS NULL OR actor_id = $4)
ORDER BY at DESC, id DESC
LIMIT $5 OFFSET $6;

-- name: CountAuditEvents :one
-- Matching total for the same filter set ListAuditEvents reads.
-- Surfaces the absolute count so the UI can render a
-- "showing X–Y of Z" header + Prev/Next bounds without a second
-- guess-and-check fetch.
SELECT COUNT(*)::bigint AS total
FROM audit_events
WHERE ($1::TEXT = '' OR action = $1)
  AND ($2::TEXT = '' OR target_type = $2)
  AND ($3::TEXT = '' OR actor_email ILIKE '%' || $3 || '%')
  AND ($4::UUID IS NULL OR actor_id = $4);
