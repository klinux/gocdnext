-- +goose Up
-- +goose StatementBegin
-- Compliance policies are admin-defined pipeline configuration that the server
-- MERGES into the effective definition of every targeted project — mandatory
-- jobs / approval gates that repo authors cannot remove (the GitLab compliance
-- pipeline guarantee). The policy is authored in the same pipeline YAML schema
-- (stages + jobs, jobs may carry approval gates), stored as editable source in
-- config_yaml and as the compiled domain snapshot in config (JSONB).
--
-- mode:
--   inject   — policy stages/jobs are appended to the project's own pipeline.
--   override — policy stages/jobs replace the project's (GitLab "override").
--
-- priority orders multiple policies on the same project (lower applies first).
-- applies_to_all = true makes the policy enforce on EVERY project regardless of
-- framework (a global baseline); otherwise it applies only to projects carrying
-- a framework listed in policy_frameworks.
CREATE TABLE compliance_policies (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL UNIQUE,
    description    TEXT NOT NULL DEFAULT '',
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    mode           TEXT NOT NULL DEFAULT 'inject',
    priority       INT NOT NULL DEFAULT 0,
    applies_to_all BOOLEAN NOT NULL DEFAULT FALSE,
    -- position_before / position_after anchor the policy's injected stages
    -- relative to an existing project stage (e.g. an approval gate
    -- position_before='deploy'). Both empty = prepend (compliance runs first).
    -- Ignored in override mode. Mutually exclusive (validated server-side).
    position_before TEXT NOT NULL DEFAULT '',
    position_after  TEXT NOT NULL DEFAULT '',
    config_yaml    TEXT NOT NULL,
    config         JSONB NOT NULL,
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT compliance_policies_mode_check
        CHECK (mode IN ('inject', 'override'))
);

-- policy_frameworks scopes a policy to specific frameworks (ignored when the
-- policy is applies_to_all). The framework_id index powers the recompute
-- fan-out alongside project_frameworks.
CREATE TABLE policy_frameworks (
    policy_id    UUID NOT NULL REFERENCES compliance_policies(id) ON DELETE CASCADE,
    framework_id UUID NOT NULL REFERENCES compliance_frameworks(id) ON DELETE CASCADE,
    PRIMARY KEY (policy_id, framework_id)
);

CREATE INDEX idx_policy_frameworks_framework ON policy_frameworks (framework_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE policy_frameworks;
DROP TABLE compliance_policies;
-- +goose StatementEnd
