-- +goose Up
-- +goose StatementBegin

-- Per-git-material polling bookkeeping. The poller worker reads
-- the material's JSONB config (poll_interval nanoseconds), asks
-- the git provider for the current branch HEAD, and compares
-- against last_head_sha here. When they differ, a modification
-- is inserted (same path as a webhook delivery) and last_head_sha
-- advances. last_polled_at gates the next tick — a material is
-- due only when (last_polled_at + poll_interval) <= now.
--
-- last_poll_error captures the last provider error (auth failure,
-- branch not found) so /projects UI can surface it instead of
-- silently looking idle. Cleared when a poll succeeds.
--
-- ON DELETE CASCADE on material_id cleans up when the pipeline
-- drops the material or the project is deleted.
CREATE TABLE material_poll_state (
    material_id     UUID PRIMARY KEY REFERENCES materials(id) ON DELETE CASCADE,
    last_polled_at  TIMESTAMPTZ,
    last_head_sha   TEXT,
    last_poll_error TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Project-level polling default. The scm_source is a 1:1 with the
-- project's repo binding, so this is the natural home for the
-- polling fallback applied to the synthesized implicit project
-- material (see injectImplicitProjectMaterial). Stored in
-- nanoseconds to match the JSONB domain encoding.
ALTER TABLE scm_sources
    ADD COLUMN poll_interval_ns BIGINT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE scm_sources DROP COLUMN IF EXISTS poll_interval_ns;
DROP TABLE IF EXISTS material_poll_state;
-- +goose StatementEnd
