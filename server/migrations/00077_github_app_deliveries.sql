-- +goose Up

-- Idempotency ledger for GitHub App webhook deliveries (the App-webhook path,
-- e.g. check_run `rerequested` — the PR "Re-run" button). GitHub redelivers on
-- transient failures, and two redeliveries can race; keying on the opaque
-- X-GitHub-Delivery id makes processing exactly-once.
--
-- The row is claimed AND the rerun is created in ONE transaction (see
-- store.RerunForAppDelivery): a crash rolls back both (no orphan run, no stuck
-- claim), and a duplicate/concurrent delivery loses the PK-insert race so its
-- transaction rolls back — never a second run. run_id is nullable +
-- ON DELETE SET NULL so pruning a run doesn't strand the ledger.
CREATE TABLE github_app_deliveries (
    delivery_id TEXT PRIMARY KEY,
    event       TEXT NOT NULL,
    run_id      UUID REFERENCES runs(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Supports the retention sweep (DELETE WHERE created_at < cutoff).
CREATE INDEX idx_github_app_deliveries_created ON github_app_deliveries(created_at);

-- The App-webhook path resolves the candidate secret by the payload's App id
-- (check_run.app.id → vcs_integrations.app_id). app_id is NOT unique today
-- (only name is), so enforce one ENABLED row per GitHub App id — the resolver
-- fails closed on a duplicate. Scoped to enabled=TRUE so a legacy DISABLED App
-- sharing an app_id with the active one can't fail this migration.
CREATE UNIQUE INDEX idx_vcs_integrations_app_id
    ON vcs_integrations(app_id)
    WHERE kind = 'github_app' AND app_id IS NOT NULL AND enabled = TRUE;

-- +goose Down

DROP INDEX IF EXISTS idx_vcs_integrations_app_id;
DROP TABLE IF EXISTS github_app_deliveries;
