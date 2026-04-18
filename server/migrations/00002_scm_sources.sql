-- +goose Up
-- +goose StatementBegin

-- A project is bound to an SCM source (typically a git repo) which carries
-- the `.gocdnext/` folder. The webhook that lands on this repo's default
-- branch is what the server will use (in a later slice) to re-sync the
-- pipeline definitions automatically — "config drift" detection.
--
-- 1:1 with projects for now. Moving to 1:N (one project with pipelines in
-- multiple repos) is a non-breaking change: drop the UNIQUE, add routing
-- metadata per-source.
CREATE TABLE scm_sources (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id             UUID NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
    provider               TEXT NOT NULL CHECK (provider IN ('github', 'gitlab', 'bitbucket', 'manual')),
    url                    TEXT NOT NULL,
    default_branch         TEXT NOT NULL DEFAULT 'main',
    webhook_secret         TEXT,                 -- HMAC shared secret (nullable when manual)
    auth_ref               TEXT,                 -- opaque pointer to the stored credential (PAT, OAuth token id)
    last_synced_at         TIMESTAMPTZ,
    last_synced_revision   TEXT,                 -- commit SHA at last sync
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_scm_sources_url ON scm_sources(url);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS scm_sources;
-- +goose StatementEnd
