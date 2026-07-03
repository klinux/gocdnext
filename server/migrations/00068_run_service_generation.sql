-- +goose Up
-- +goose StatementBegin

-- Per-run service-pod generation (#97). Every run starts at 0; RerunJob bumps it
-- each time a TERMINAL run is revived under the SAME run_id. The Kubernetes engine
-- stamps the generation into each `services:` pod's name + a generation label, so a
-- revived run builds a FRESH pod set and a stale supersede/terminal CleanupRunServices
-- — which carries the older generation it decided to tear down — cannot delete the
-- revived generation's pods. Closes the cleanup-vs-revive race that a wall-clock
-- created_before could not (pod reuse by deterministic name reused a pre-cutoff pod).
--
-- A full-run rerun (RerunRun) already mints a new run_id (distinct pod names), so only
-- the same-run_id RerunJob path needs this. Existing runs backfill to 0 via the
-- DEFAULT — correct, since no pre-migration pod carries a generation label and the
-- agent treats a missing label as generation 0.
ALTER TABLE runs ADD COLUMN service_generation BIGINT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN IF EXISTS service_generation;
-- +goose StatementEnd
