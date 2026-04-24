-- +goose Up
-- +goose StatementBegin

-- Audit log of operator + system actions. Every write the RBAC
-- layer gates (apply, secrets, approvals, triggers, role changes,
-- etc.) emits one row so ops can answer "who did X and when".
--
-- actor_id is nullable for system-driven events (cron triggers,
-- webhook-auto-created runs) so the schema doesn't force a
-- "system" sentinel user. actor_email is denormalised on insert
-- so the log stays readable even after the user row is deleted.
-- target_type + target_id pair addresses anything — project,
-- run, secret, user, cache — without forcing per-target FKs that
-- would cascade-delete history when the target is cleaned up.
-- metadata JSONB carries the extra shape per action
-- (e.g. {"from": "maintainer", "to": "admin"} on role change).

CREATE TABLE audit_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id     UUID,
    actor_email  TEXT NOT NULL DEFAULT '',
    action       TEXT NOT NULL,
    target_type  TEXT NOT NULL,
    target_id    TEXT NOT NULL DEFAULT '',
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
    at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary listing index: the admin page shows a reverse-chrono
-- stream with optional filters, so the common query is
-- `ORDER BY at DESC LIMIT N` with optional `WHERE actor_id=$`
-- / `WHERE action=$`. Indexing on `at DESC` covers all of them
-- via an index-only scan for the default "recent activity" view.
CREATE INDEX idx_audit_events_at ON audit_events (at DESC);

-- Covering index for per-actor drill-down ("what did alice do?").
-- Includes `at` so ordering stays cheap inside the filter.
CREATE INDEX idx_audit_events_actor_at
    ON audit_events (actor_id, at DESC)
    WHERE actor_id IS NOT NULL;

-- Per-action filter — "every approve this month" / "every
-- secret delete in April".
CREATE INDEX idx_audit_events_action_at
    ON audit_events (action, at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_events;
-- +goose StatementEnd
