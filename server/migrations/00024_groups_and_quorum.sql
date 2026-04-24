-- +goose Up
-- +goose StatementBegin

-- Groups: lightweight organisational buckets that can approve
-- a gate as a collective. Orthogonal to role (admin/maintainer/
-- viewer) — role is a system permission; group is a logical
-- grouping ("SRE", "release managers") that approval gates
-- reference by name.
--
-- Membership is many-to-many: a user can sit in any number of
-- groups, a group can hold any number of users.
CREATE TABLE groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE group_members (
    group_id UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  UUID NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    added_by UUID REFERENCES users(id) ON DELETE SET NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, user_id)
);
-- Reverse lookup: "which groups is this user in?" — used on
-- every approve call to check if a user's groups intersect
-- the gate's approver_groups list.
CREATE INDEX idx_group_members_user ON group_members(user_id);

-- Approval-gate extensions on job_runs:
--  - approver_groups: group names (same string-by-name pattern
--    as approvers — the YAML references groups by name, so we
--    persist names to keep the gate resilient to group renames
--    at decision time).
--  - approval_required: quorum. 1 = today's behaviour (any single
--    approver ok); 2+ = N approvers required. First reject from
--    any allowed user still kills the gate immediately.
ALTER TABLE job_runs
    ADD COLUMN approver_groups   TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN approval_required INT    NOT NULL DEFAULT 1
        CHECK (approval_required >= 1);

-- Individual votes — each approved/rejected decision is one row.
-- Unique (job_run_id, user_id) so a user can't pad the quorum by
-- double-voting. The final job_runs.decision/decided_by stays as
-- a summary (quorum-hit approver or the rejecter) for the UI's
-- "who ended this" view; this table carries the detail trail.
CREATE TABLE job_run_approvals (
    job_run_id UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id)   ON DELETE CASCADE,
    user_label TEXT NOT NULL DEFAULT '',
    decision   TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
    comment    TEXT NOT NULL DEFAULT '',
    decided_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_run_id, user_id)
);
CREATE INDEX idx_job_run_approvals_job_run ON job_run_approvals(job_run_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS job_run_approvals;
ALTER TABLE job_runs DROP COLUMN IF EXISTS approval_required;
ALTER TABLE job_runs DROP COLUMN IF EXISTS approver_groups;
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
-- +goose StatementEnd
