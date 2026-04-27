-- +goose Up
-- +goose StatementBegin

-- Runner profiles are admin-defined named bundles of execution
-- policy: which engine, which fallback image, default + max
-- compute resources, and the agent tags this profile may run on.
-- Pipelines reference profiles by name (`agent.profile: gpu` in
-- YAML); the scheduler resolves the name to the live row at
-- dispatch time so admins can update the profile without re-
-- applying every pipeline.
--
-- Generic columns (default_image, default_*_request/limit,
-- max_*, tags) are honoured by every engine that understands
-- those concepts. `config JSONB` is engine-specific overflow:
-- empty in v0 (Level 1 deliberately keeps the surface tiny),
-- but reserved so future engines (or future fields within
-- kubernetes — nodeSelector, etc) extend without a migration.

CREATE TABLE runner_profiles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    engine TEXT NOT NULL,
    default_image TEXT NOT NULL DEFAULT '',
    default_cpu_request TEXT NOT NULL DEFAULT '',
    default_cpu_limit   TEXT NOT NULL DEFAULT '',
    default_mem_request TEXT NOT NULL DEFAULT '',
    default_mem_limit   TEXT NOT NULL DEFAULT '',
    max_cpu TEXT NOT NULL DEFAULT '',
    max_mem TEXT NOT NULL DEFAULT '',
    tags TEXT[] NOT NULL DEFAULT '{}',
    config JSONB NOT NULL DEFAULT '{}'::JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT runner_profiles_engine_check
        CHECK (engine IN ('kubernetes'))
);

CREATE INDEX runner_profiles_engine_idx ON runner_profiles(engine);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS runner_profiles;

-- +goose StatementEnd
