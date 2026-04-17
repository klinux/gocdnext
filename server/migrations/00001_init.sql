-- +goose Up
-- +goose StatementBegin

-- Projects agrupam pipelines (ex: 1 por repo, ou 1 por time)
CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Pipelines são parsed do YAML ou criados via API
CREATE TABLE pipelines (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    definition   JSONB NOT NULL,             -- snapshot canônico do YAML parseado
    definition_version INT NOT NULL DEFAULT 1,
    config_repo  TEXT,                       -- se veio de um config-repo externo
    config_path  TEXT NOT NULL DEFAULT '.gocdnext.yaml',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);

-- Materials: git, upstream, cron, manual
CREATE TABLE materials (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_id   UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    type          TEXT NOT NULL CHECK (type IN ('git', 'upstream', 'cron', 'manual')),
    config        JSONB NOT NULL,
    fingerprint   TEXT NOT NULL,             -- hash estável para dedup
    auto_update   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pipeline_id, fingerprint)
);

CREATE INDEX idx_materials_fingerprint ON materials(fingerprint);

-- Modificações (commits, upstream run finalizados)
CREATE TABLE modifications (
    id           BIGSERIAL PRIMARY KEY,
    material_id  UUID NOT NULL REFERENCES materials(id) ON DELETE CASCADE,
    revision     TEXT NOT NULL,
    branch       TEXT,
    author       TEXT,
    message      TEXT,
    payload      JSONB,                      -- payload original do webhook
    committed_at TIMESTAMPTZ,
    detected_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (material_id, revision, branch)
);

CREATE INDEX idx_modifications_material_detected ON modifications(material_id, detected_at DESC);

-- Runs: execuções de pipeline
CREATE TABLE runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_id  UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    counter      BIGINT NOT NULL,
    cause        TEXT NOT NULL,               -- webhook|upstream|manual|schedule|poll
    cause_detail JSONB,
    status       TEXT NOT NULL DEFAULT 'queued',
    revisions    JSONB NOT NULL,              -- snapshot dos materials no momento
    triggered_by TEXT,                        -- user id / system
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pipeline_id, counter)
);

CREATE INDEX idx_runs_pipeline_counter ON runs(pipeline_id, counter DESC);
CREATE INDEX idx_runs_status ON runs(status) WHERE status IN ('queued', 'running');

-- Stages de um run (ordem herdada da pipeline)
CREATE TABLE stage_runs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    ordinal     INT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'queued',
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (run_id, name)
);

-- Jobs executados por agents
CREATE TABLE job_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    stage_run_id  UUID NOT NULL REFERENCES stage_runs(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    matrix_key    TEXT,                      -- "OS=linux,ARCH=amd64" se matrix
    image         TEXT,
    agent_id      UUID,                      -- assinado quando despachado
    status        TEXT NOT NULL DEFAULT 'queued',
    exit_code     INT,
    error         TEXT,
    needs         TEXT[] NOT NULL DEFAULT '{}',
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);

CREATE INDEX idx_job_runs_status ON job_runs(status) WHERE status IN ('queued', 'running');
CREATE INDEX idx_job_runs_agent ON job_runs(agent_id) WHERE agent_id IS NOT NULL;

-- Agents registrados
CREATE TABLE agents (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL UNIQUE,
    token_hash     TEXT NOT NULL,
    version        TEXT,
    os             TEXT,
    arch           TEXT,
    tags           TEXT[] NOT NULL DEFAULT '{}',
    capacity       INT NOT NULL DEFAULT 1,
    status         TEXT NOT NULL DEFAULT 'offline',  -- online|offline|disabled
    last_seen_at   TIMESTAMPTZ,
    registered_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Log lines (append-only). Para MVP, Postgres; depois migrar para ClickHouse/Loki.
CREATE TABLE log_lines (
    id         BIGSERIAL PRIMARY KEY,
    job_run_id UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    stream     TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    text       TEXT NOT NULL,
    UNIQUE (job_run_id, seq)
);

CREATE INDEX idx_log_lines_job_seq ON log_lines(job_run_id, seq);

-- Artefatos produzidos por jobs
CREATE TABLE artifacts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_run_id   UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    storage_url  TEXT NOT NULL,
    size_bytes   BIGINT,
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Webhook deliveries (observabilidade — Fase 1)
CREATE TABLE webhook_deliveries (
    id           BIGSERIAL PRIMARY KEY,
    provider     TEXT NOT NULL,
    event        TEXT NOT NULL,
    material_id  UUID REFERENCES materials(id) ON DELETE SET NULL,
    status       TEXT NOT NULL,               -- accepted|rejected|error
    http_status  INT NOT NULL,
    headers      JSONB,
    payload      JSONB,
    error        TEXT,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_webhook_deliveries_received ON webhook_deliveries(received_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS log_lines;
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS job_runs;
DROP TABLE IF EXISTS stage_runs;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS modifications;
DROP TABLE IF EXISTS materials;
DROP TABLE IF EXISTS pipelines;
DROP TABLE IF EXISTS projects;
-- +goose StatementEnd
