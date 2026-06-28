-- +goose Up
-- +goose StatementBegin

-- vcs_pull_requests records the lifecycle timestamps of a pull/merge request so
-- DORA "lead time for changes" can later be decomposed into Coding (first
-- commit → PR opened) and Review (PR opened → approval/merge) stages. One row
-- per (provider, repo, number); webhook handlers upsert opened/approved/merged
-- as the events arrive (any order). merge_sha correlates the merged change to
-- the commit a deployment ships.
CREATE TABLE vcs_pull_requests (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider    TEXT NOT NULL,
    repo        TEXT NOT NULL,                 -- "owner/name" (or clone URL)
    number      BIGINT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL DEFAULT '',
    head_ref    TEXT NOT NULL DEFAULT '',
    base_ref    TEXT NOT NULL DEFAULT '',
    head_sha    TEXT NOT NULL DEFAULT '',
    merge_sha   TEXT NOT NULL DEFAULT '',      -- commit merged to base (→ deploy)
    opened_at   TIMESTAMPTZ,                   -- PR created_at (Coding boundary)
    approved_at TIMESTAMPTZ,                   -- first approving review (Review)
    merged_at   TIMESTAMPTZ,                   -- merged into base
    closed_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, repo, number)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Phase 2 correlates a deployed commit to its PR via the merge SHA.
CREATE INDEX idx_vcs_pull_requests_merge_sha
    ON vcs_pull_requests (merge_sha) WHERE merge_sha <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS vcs_pull_requests;
-- +goose StatementEnd
