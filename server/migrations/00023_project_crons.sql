-- +goose Up
-- +goose StatementBegin

-- Project-level scheduled fires. Independent of cron materials
-- (which live inside a single pipeline's YAML); these fire N
-- pipelines of a project on a shared schedule. Usecase:
-- "nightly at 2am, build + deploy everything in this project"
-- without having to paste the same cron into every pipeline's
-- YAML.
--
-- pipeline_ids is empty = "all pipelines in this project (at
-- fire time)". A non-empty list pins the targets — added
-- pipelines don't implicitly join the schedule, dropped ones
-- fall out silently (missing id skipped at fire time, no error).
--
-- Ownership: cascade on project delete — dropping a project
-- nukes its schedules too.
CREATE TABLE project_crons (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    expression     TEXT NOT NULL,
    pipeline_ids   UUID[] NOT NULL DEFAULT '{}',
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    last_fired_at  TIMESTAMPTZ,
    created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);

CREATE INDEX idx_project_crons_project ON project_crons(project_id);
-- Ticker walks enabled rows each tick; partial index keeps the
-- scan tight when most schedules are disabled.
CREATE INDEX idx_project_crons_enabled ON project_crons(enabled) WHERE enabled = TRUE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_crons;
-- +goose StatementEnd
