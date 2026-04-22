-- +goose Up
-- +goose StatementBegin

-- Per-cron-material trigger bookkeeping. The cron ticker records
-- the last fire time here so restarting the server doesn't double-
-- trigger a schedule that fired seconds before the restart, and
-- so a tick that lands inside the same minute as the previous one
-- doesn't re-fire the same expression.
--
-- materials.ON DELETE CASCADE takes care of cleanup when a
-- pipeline drops its cron material.
CREATE TABLE cron_state (
    material_id   UUID PRIMARY KEY REFERENCES materials(id) ON DELETE CASCADE,
    last_fired_at TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS cron_state;
-- +goose StatementEnd
