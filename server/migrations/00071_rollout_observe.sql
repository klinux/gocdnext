-- +goose Up
-- +goose StatementBegin

-- Rollout observation (ADR-0001 Phase 2, PR1 — observe-only). A rollout-aware
-- deploy target additionally reads the Argo Rollouts Rollout CR the Application
-- manages; the watcher persists the observed snapshot onto the deploy_watch so the
-- UI (which reads the DB, not the watcher's memory) can render canary progress.
-- Gate control (governing_gate + the gate/action columns) lands in PR2.

-- deploy_targets: turn rollout observation on + optionally pin the Rollout's
-- location. rollout_cluster empty/NULL => the Application's own cluster; namespace/
-- name empty/NULL => auto-discover the single Rollout from the Application's
-- managed resources. rollout_cluster FKs the immutable cluster name (RESTRICT), same
-- as `cluster`.
ALTER TABLE deploy_targets
    ADD COLUMN rollout_aware     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN rollout_cluster   TEXT NULL REFERENCES clusters(name) ON DELETE RESTRICT,
    ADD COLUMN rollout_namespace TEXT NULL,
    ADD COLUMN rollout_name      TEXT NULL;

-- deploy_watches: the denormalized rollout routing (so targetOf rebuilds a complete
-- target without re-reading the target) + the observed snapshot, restamped each tick.
-- All snapshot columns are NULLABLE = "not observed yet"; rollout_current_step is
-- INT NULL because an absent controller step index must not be read as step 0
-- (NULL <=> RolloutState.CurrentStepKnown = false).
ALTER TABLE deploy_watches
    ADD COLUMN rollout_aware        BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN rollout_cluster      TEXT NULL,
    ADD COLUMN rollout_namespace    TEXT NULL,
    ADD COLUMN rollout_name         TEXT NULL,
    ADD COLUMN rollout_phase        TEXT NULL,
    ADD COLUMN rollout_message      TEXT NULL,
    ADD COLUMN rollout_pause_reason TEXT NULL,
    ADD COLUMN rollout_current_step INT NULL,
    ADD COLUMN rollout_step_count   INT NULL,
    ADD COLUMN rollout_aborted      BOOLEAN NULL,
    ADD COLUMN rollout_error        TEXT NULL,
    ADD COLUMN rollout_observed_at  TIMESTAMPTZ NULL;

-- +goose StatementEnd
