-- +goose Up
-- Per-service-per-run tracking row. The agent emits
-- ServiceLifecycle messages whenever a service Pod transitions
-- state; the server upserts here, keyed by (run_id, name), so
-- the UI can render services as nodes alongside jobs (status,
-- duration, ready window) and the API can answer
-- GET /api/runs/{id}/services.
--
-- status is intentionally a TEXT (not an enum) so a future
-- value (e.g. "evicted") doesn't require an ALTER TYPE +
-- migration rollout across the fleet.
CREATE TABLE service_runs (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id     UUID        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  name       TEXT        NOT NULL,
  image      TEXT        NOT NULL,
  pod_name   TEXT        NOT NULL DEFAULT '',
  status     TEXT        NOT NULL,
  started_at TIMESTAMPTZ,
  ready_at   TIMESTAMPTZ,
  stopped_at TIMESTAMPTZ,
  error      TEXT        NOT NULL DEFAULT '',
  UNIQUE (run_id, name)
);

-- Idx for the API endpoint that lists every service of a run +
-- the run-detail UI's lookup. Partial on stopped_at IS NULL is
-- tempting (only "live" services) but the UI shows STOPPED ones
-- too as part of the run timeline, so a covering full index is
-- the right shape.
CREATE INDEX idx_service_runs_run_id ON service_runs (run_id);

-- +goose Down
DROP TABLE service_runs;
