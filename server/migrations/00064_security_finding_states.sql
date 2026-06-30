-- +goose Up

-- #71 v2: cross-run finding identity + state. v1 stores findings per scan but
-- does nothing across runs. This adds a per-identity table that persists
-- seen-tracking (new/existing/fixed) and the user state (dismiss/FP/accepted),
-- plus denormalizes the scanner grain onto the v1 tables so reads stop joining
-- job_runs.

-- +goose StatementBegin
-- Carry the matrix cell on the finding row (the identity needs it, and the read
-- joins by it). v1 left it on job_runs only.
ALTER TABLE security_findings ADD COLUMN matrix_key TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
-- The latest-scan grain is (pipeline, scanner_job, matrix_key); denormalize both
-- onto the marker so the latest-scan CTE groups by scanner without joining
-- job_runs.
ALTER TABLE security_scans
    ADD COLUMN scanner_job TEXT NOT NULL DEFAULT '',
    ADD COLUMN matrix_key  TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE security_findings f
SET matrix_key = COALESCE(jr.matrix_key, '')
FROM job_runs jr
WHERE jr.id = f.job_run_id;
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE security_scans sc
SET scanner_job = jr.name,
    matrix_key  = COALESCE(jr.matrix_key, '')
FROM job_runs jr
WHERE jr.id = sc.job_run_id;
-- +goose StatementEnd

-- +goose StatementBegin
-- The latest-scan CTEs now group/sort by (pipeline_id, scanner_job, matrix_key).
-- Replace the old (pipeline_id)-only index with the composite (its leftmost
-- prefix still serves any pipeline_id-only lookup). A future optimization, if
-- this page gets hot, is denormalizing runs.counter onto security_scans for an
-- index-only latest selection.
DROP INDEX IF EXISTS idx_security_scans_pipeline;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_security_scans_scanner
    ON security_scans (pipeline_id, scanner_job, matrix_key);
-- +goose StatementEnd

-- +goose StatementBegin
-- One row per finding identity (pipeline + scanner job + matrix cell + tool +
-- fingerprint). tool is in the key because one job can publish multiple SARIF
-- tools whose fingerprints carry different meaning. Holds seen-tracking, the
-- user state (persists across reruns), and a snapshot of the last occurrence so
-- the "fixed" list can render findings whose security_findings row is gone.
CREATE TABLE security_finding_states (
    id            BIGSERIAL PRIMARY KEY,
    project_id    UUID NOT NULL,
    pipeline_id   UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    scanner_job   TEXT NOT NULL,
    matrix_key    TEXT NOT NULL DEFAULT '',
    tool          TEXT NOT NULL,
    fingerprint   TEXT NOT NULL,
    first_seen_run_id UUID REFERENCES runs(id) ON DELETE SET NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_run_id  UUID REFERENCES runs(id) ON DELETE SET NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- last-occurrence snapshot (the fixed list's row is gone from security_findings)
    last_rule_id       TEXT NOT NULL DEFAULT '',
    last_severity      TEXT NOT NULL DEFAULT '',
    last_level         TEXT NOT NULL DEFAULT '',
    last_message       TEXT NOT NULL DEFAULT '',
    last_location_path TEXT NOT NULL DEFAULT '',
    last_location_line INT  NOT NULL DEFAULT 0,
    state         TEXT NOT NULL DEFAULT 'open'
        CHECK (state IN ('open','dismissed','false_positive','accepted')),
    state_reason  TEXT NOT NULL DEFAULT '',
    -- no FK on state_actor_id: the actor may be a local/OIDC user OR a service
    -- account; keep it flexible + carry an email snapshot for audit display.
    state_actor_id    UUID,
    state_actor_email TEXT NOT NULL DEFAULT '',
    state_updated_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pipeline_id, scanner_job, matrix_key, tool, fingerprint)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_finding_states_project ON security_finding_states (project_id);
-- +goose StatementEnd

-- +goose StatementBegin
-- Per-scanner / fixed read path: the fixed query joins by (pipeline, scanner_job,
-- matrix_key) against the latest scan.
CREATE INDEX idx_finding_states_scanner
    ON security_finding_states (pipeline_id, scanner_job, matrix_key);
-- +goose StatementEnd

-- +goose StatementBegin
-- Backfill identities from the findings v1 already shipped — without this the
-- joined read would blank out current findings until each pipeline's next scan.
-- first/last-seen come from the occurrences' own timestamps (NOT now()); the
-- snapshot is the latest occurrence, chosen deterministically.
INSERT INTO security_finding_states (
    project_id, pipeline_id, scanner_job, matrix_key, tool, fingerprint,
    first_seen_run_id, first_seen_at, last_seen_run_id, last_seen_at,
    last_rule_id, last_severity, last_level, last_message,
    last_location_path, last_location_line
)
WITH ranked AS (
    SELECT
        f.project_id, f.pipeline_id, f.job_name AS scanner_job, f.matrix_key,
        f.tool, f.fingerprint, f.run_id, f.created_at,
        f.rule_id, f.severity, f.level, f.message, f.location_path, f.location_line,
        ROW_NUMBER() OVER (
            PARTITION BY f.pipeline_id, f.job_name, f.matrix_key, f.tool, f.fingerprint
            ORDER BY r.counter DESC, f.created_at DESC, f.id DESC
        ) AS rn_last,
        ROW_NUMBER() OVER (
            PARTITION BY f.pipeline_id, f.job_name, f.matrix_key, f.tool, f.fingerprint
            ORDER BY r.counter ASC, f.created_at ASC, f.id ASC
        ) AS rn_first
    FROM security_findings f
    JOIN runs r ON r.id = f.run_id
)
SELECT
    last.project_id, last.pipeline_id, last.scanner_job, last.matrix_key,
    last.tool, last.fingerprint,
    first.run_id, first.created_at,
    last.run_id, last.created_at,
    last.rule_id, last.severity, last.level, last.message,
    last.location_path, last.location_line
FROM (SELECT * FROM ranked WHERE rn_last = 1) last
JOIN (SELECT * FROM ranked WHERE rn_first = 1) first
    ON  first.pipeline_id = last.pipeline_id
    AND first.scanner_job = last.scanner_job
    AND first.matrix_key  = last.matrix_key
    AND first.tool        = last.tool
    AND first.fingerprint = last.fingerprint
ON CONFLICT (pipeline_id, scanner_job, matrix_key, tool, fingerprint) DO NOTHING;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP TABLE IF EXISTS security_finding_states;
-- +goose StatementEnd

-- +goose StatementBegin
-- Dropping the columns auto-removes idx_security_scans_scanner; restore the
-- original (pipeline_id)-only index.
ALTER TABLE security_scans DROP COLUMN IF EXISTS scanner_job, DROP COLUMN IF EXISTS matrix_key;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_security_scans_pipeline ON security_scans (pipeline_id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE security_findings DROP COLUMN IF EXISTS matrix_key;
-- +goose StatementEnd
