-- +goose Up

-- Idempotency ledger for GitHub App webhook deliveries (the App-webhook path,
-- e.g. check_run/check_suite `rerequested` — the PR "Re-run" button). GitHub
-- redelivers on transient failures, and two redeliveries can race; keying on
-- the opaque X-GitHub-Delivery id makes processing exactly-once.
--
--   status='processing' — claimed, work in flight (or a crashed attempt)
--   status='done'       — the rerun landed (run_id points at it)
--
-- The PK on delivery_id makes the claim atomic (INSERT ... ON CONFLICT DO
-- NOTHING): concurrent redeliveries can't both claim. A crash between claim and
-- done leaves a stale 'processing' row, which a later redelivery re-claims once
-- older than a short TTL (scanned via the (status, updated_at) index). run_id is
-- nullable + ON DELETE SET NULL so pruning a run doesn't strand the ledger.
CREATE TABLE github_app_deliveries (
    delivery_id TEXT PRIMARY KEY,
    event       TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'processing'
        CHECK (status IN ('processing', 'done')),
    run_id      UUID REFERENCES runs(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_github_app_deliveries_stale
    ON github_app_deliveries(status, updated_at);

-- The App-webhook path resolves the candidate secret by the payload's App id
-- (check_run.app.id → vcs_integrations.app_id). app_id is NOT unique today
-- (only name is), so enforce one row per GitHub App id — the resolver fails
-- closed on a duplicate.
CREATE UNIQUE INDEX idx_vcs_integrations_app_id
    ON vcs_integrations(app_id)
    WHERE kind = 'github_app' AND app_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_vcs_integrations_app_id;
DROP TABLE IF EXISTS github_app_deliveries;
