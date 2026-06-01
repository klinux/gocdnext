-- name: FindAgentByName :one
SELECT id, name, token_hash, version, os, arch, tags, capacity, status, last_seen_at, registered_at, session_generation, engine
FROM agents
WHERE name = $1
LIMIT 1;

-- name: InsertAgent :one
INSERT INTO agents (name, token_hash, version, os, arch, tags, capacity, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, name, token_hash, version, os, arch, tags, capacity, status, last_seen_at, registered_at, session_generation, engine;

-- name: UpdateAgentOnRegister :one
-- Bumps the per-agent session_generation counter atomically with
-- the online-stamp. The new generation flows back to the caller
-- (Connect handler) which captures it for its eventual
-- MarkAgentOffline defer.
--
-- Why this is monotonic int rather than session_id (TEXT):
-- session_id is a bearer credential used by Connect's auth — the
-- in-memory SessionStore.Lookup compares raw strings, so anything
-- holding the id can impersonate. Persisting it would mean a
-- read-only DB leak (backup, snapshot, log) effectively dumps live
-- session tokens. The CAS only needs an epoch indicator — a
-- counter carries exactly that signal with no auth power.
UPDATE agents
SET version            = $2,
    os                 = $3,
    arch               = $4,
    tags               = $5,
    capacity           = $6,
    engine             = $7,
    status             = 'online',
    last_seen_at       = NOW(),
    session_generation = session_generation + 1
WHERE id = $1
RETURNING session_generation;

-- name: MarkAgentOffline :exec
-- Generation-aware offline mark. Only flips status when the
-- closing handler's observed generation still matches the row.
-- A successor Register bumps session_generation, so an old
-- defer (which captured the prior value) finds no rows to
-- update and no-ops — preserving the successor's online state.
UPDATE agents
SET status = 'offline'
WHERE id = $1
  AND session_generation = @observed_generation::bigint;
