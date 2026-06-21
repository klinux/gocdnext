-- +goose Up
-- +goose StatementBegin
-- Compliance frameworks are admin-defined LABELS (e.g. "SOC2", "PCI",
-- "internal") assigned to projects. gocdnext has no group hierarchy, so a
-- framework is the scoping unit for compliance policies: a policy targets one
-- or more frameworks, and the server enforces it on every project carrying a
-- matching framework. This is the gocdnext equivalent of GitLab's compliance
-- frameworks (which are group-level labels).
CREATE TABLE compliance_frameworks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- project_frameworks assigns frameworks to projects (many-to-many). The
-- framework_id index powers the recompute fan-out: when a policy or framework
-- changes, we re-merge the effective pipeline definition for every project
-- carrying the affected framework.
CREATE TABLE project_frameworks (
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    framework_id UUID NOT NULL REFERENCES compliance_frameworks(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, framework_id)
);

CREATE INDEX idx_project_frameworks_framework ON project_frameworks (framework_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE project_frameworks;
DROP TABLE compliance_frameworks;
-- +goose StatementEnd
