-- name: UpsertServiceRun :one
-- Upsert the service tracking row for a (run_id, name) tuple.
-- Idempotent: re-issuing the same status is a no-op besides
-- updating started_at/ready_at/stopped_at as appropriate.
--
-- Status transition shape:
--   starting → ready → stopped
--          ↘ failed   ↗ stopped
-- We don't enforce the order in SQL — the agent is the source of
-- truth, and a malformed sequence (e.g. `ready` arriving before
-- `starting` after a stream reconnect) is harmless: the row ends
-- up with both timestamps set, the UI renders the later state.
-- Guarding ready_at and stopped_at with COALESCE means an idempotent
-- re-send of `ready` doesn't reset the first-observed timestamp.
INSERT INTO service_runs (
    run_id, name, image, pod_name, status,
    started_at, ready_at, stopped_at, error
) VALUES (
    @run_id, @name, @image, @pod_name, @status,
    CASE WHEN @status::TEXT IN ('starting','ready') THEN @at::TIMESTAMPTZ ELSE NULL END,
    CASE WHEN @status::TEXT = 'ready'   THEN @at::TIMESTAMPTZ ELSE NULL END,
    CASE WHEN @status::TEXT IN ('stopped','failed') THEN @at::TIMESTAMPTZ ELSE NULL END,
    @error
)
ON CONFLICT (run_id, name) DO UPDATE SET
    image      = EXCLUDED.image,
    pod_name   = CASE WHEN EXCLUDED.pod_name <> '' THEN EXCLUDED.pod_name ELSE service_runs.pod_name END,
    -- `failed` is sticky: once a service is observed failing, a
    -- subsequent `stopped` from the cleanup broadcast (which fires
    -- on EVERY run, including the failed ones) MUST NOT erase the
    -- failure signal in the UI. Without this CASE, the order
    -- failed → stopped (which is the normal flow when a pod blows
    -- up and cleanup later sweeps it) would show as `stopped` and
    -- hide the actual root cause.
    status     = CASE
        WHEN service_runs.status = 'failed' THEN service_runs.status
        ELSE EXCLUDED.status
    END,
    started_at = COALESCE(service_runs.started_at, EXCLUDED.started_at),
    ready_at   = COALESCE(service_runs.ready_at,   EXCLUDED.ready_at),
    stopped_at = COALESCE(service_runs.stopped_at, EXCLUDED.stopped_at),
    error      = CASE WHEN EXCLUDED.error <> '' THEN EXCLUDED.error ELSE service_runs.error END
RETURNING *;

-- name: AgentOwnedJobInRun :one
-- Ownership probe for ServiceLifecycle ingest. Returns true when
-- the calling agent has (now OR ever) a job_run under this run —
-- the natural binding for "this agent is part of the run, so
-- whatever it says about service lifecycle is credible".
--
-- Used as the gate on `starting`/`ready`/`failed`. For `stopped`,
-- the handler additionally accepts any agent because cleanup is
-- BROADCAST to k8s-capable agents that may never have owned a
-- job (the whole point of the broadcast model).
SELECT EXISTS(
    SELECT 1
    FROM job_runs
    WHERE run_id = $1
      AND agent_id = $2
)::BOOLEAN AS owned;

-- name: ListServiceRunsByRunID :many
-- API: GET /api/runs/{id}/services. Sort by name for a stable
-- UI ordering — the YAML's declaration order isn't preserved in
-- the upsert path so alphabetical is the only stable choice.
SELECT *
FROM service_runs
WHERE run_id = $1
ORDER BY name ASC;
