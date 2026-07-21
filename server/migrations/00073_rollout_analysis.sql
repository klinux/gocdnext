-- +goose Up
-- +goose StatementBegin

-- Rollout AnalysisRun surfacing (ADR-0001 Phase 2c, PR3 — observe-only). When a canary is
-- running a metric AnalysisRun, the watcher persists its inline state onto the deploy_watch
-- so the UI (which reads the DB) can show WHY a canary is paused/degraded (an inconclusive
-- / failed analysis), not a bare "Paused". All NULL = no active analysis this tick.
-- rollout_analysis_message is cluster-supplied, bounded like rollout_message on write.
ALTER TABLE deploy_watches
    ADD COLUMN rollout_analysis_kind    TEXT NULL,  -- "step" | "background"
    ADD COLUMN rollout_analysis_name    TEXT NULL,
    ADD COLUMN rollout_analysis_phase   TEXT NULL,  -- Pending|Running|Successful|Failed|Error|Inconclusive
    ADD COLUMN rollout_analysis_message TEXT NULL;

-- +goose StatementEnd
