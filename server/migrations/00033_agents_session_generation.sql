-- +goose Up
-- +goose StatementBegin

-- agents.session_generation is the CAS key the Connect-handler defer
-- uses to decide whether its MarkAgentOffline is still relevant.
--
-- IMPORTANT — why this is a monotonic int and NOT the session UUID:
-- the session id is a bearer credential. Connect (gRPC stream open)
-- authenticates by `SessionStore.Lookup(session_id)`; anything that
-- knows the session id passes. Persisting it in clear text would
-- mean any read-only DB leak (backup, pg_dump, snapshot, SELECT-only
-- SQL injection, an over-eager log line) effectively dumps live
-- session credentials. The CAS only needs to distinguish "is this
-- defer's epoch still current?" — a per-agent monotonic counter
-- carries exactly that signal with no auth power.
--
-- The story this column resolves:
--
--   1. Agent A registers, gets session S1. MarkAgentOnline returns
--      session_generation=1; Connect handler captures 1.
--   2. A's stream closes naturally (TCP). Defer runs Revoke(S1) —
--      removes S1 from the SessionStore IMMEDIATELY.
--   3. Successor A' registers BEFORE the defer reaches its
--      MarkAgentOffline step. MarkAgentOnline bumps the row to
--      session_generation=2.
--   4. A' calls CreateSession (publishes S2 in latestByAg).
--   5. Old defer FINALLY reaches MarkAgentOffline with observed
--      generation=1. SQL: WHERE session_generation = 1 → row's
--      session_generation=2 → no rows updated → no-op. Successor
--      stays online. Race closed.
--
-- DEFAULT 0 means "never registered" — any first-register bumps to 1.
-- Existing rows from before this migration also start at 0.
ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS session_generation BIGINT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE agents
    DROP COLUMN IF EXISTS session_generation;
-- +goose StatementEnd
