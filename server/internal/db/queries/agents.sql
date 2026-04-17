-- name: FindAgentByName :one
SELECT id, name, token_hash, version, os, arch, tags, capacity, status, last_seen_at, registered_at
FROM agents
WHERE name = $1
LIMIT 1;

-- name: InsertAgent :one
INSERT INTO agents (name, token_hash, version, os, arch, tags, capacity, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, name, token_hash, version, os, arch, tags, capacity, status, last_seen_at, registered_at;

-- name: UpdateAgentOnRegister :exec
UPDATE agents
SET version      = $2,
    os           = $3,
    arch         = $4,
    tags         = $5,
    capacity     = $6,
    status       = 'online',
    last_seen_at = NOW()
WHERE id = $1;

-- name: MarkAgentOffline :exec
UPDATE agents
SET status = 'offline'
WHERE id = $1;
