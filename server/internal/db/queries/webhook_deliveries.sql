-- name: InsertWebhookDelivery :one
-- Append-only audit row for every HTTP call that lands on
-- /api/webhooks/*. Rows outlive the request so the admin page can
-- show signature-rejected and drift-only deliveries too, not just
-- the ones that produced a modification.
INSERT INTO webhook_deliveries (
    provider, event, material_id, status, http_status, headers, payload, error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, received_at;

-- name: ListWebhookDeliveries :many
-- Admin console feed. Most recent first; indexed on received_at.
-- Filter by provider + status keep the page useful even under
-- heavy traffic. Empty string = no filter on that axis.
SELECT id, provider, event, material_id, status, http_status,
       error, received_at
FROM webhook_deliveries
WHERE (@provider_filter::text = '' OR provider = @provider_filter::text)
  AND (@status_filter::text = ''   OR status   = @status_filter::text)
ORDER BY received_at DESC
LIMIT $1 OFFSET @row_offset::bigint;

-- name: CountWebhookDeliveries :one
-- Pair for ListWebhookDeliveries so the UI can render "N of M".
SELECT COUNT(*)::bigint
FROM webhook_deliveries
WHERE (@provider_filter::text = '' OR provider = @provider_filter::text)
  AND (@status_filter::text = ''   OR status   = @status_filter::text);

-- name: GetWebhookDelivery :one
-- Expands a row with its headers + payload JSON for the drawer
-- view. Kept in a separate query so the list shot stays tiny
-- (payloads can be 100+ KB).
SELECT id, provider, event, material_id, status, http_status,
       headers, payload, error, received_at
FROM webhook_deliveries
WHERE id = $1;
