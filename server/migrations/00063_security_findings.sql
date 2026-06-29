-- +goose Up
-- +goose StatementBegin

-- Security findings ingested from SARIF scanner artifacts (#71 v1). One row per
-- finding. The full SARIF stays the retained artifact — we keep only normalized
-- fields + a pointer (artifact_id/path) for drill-down, so this table doesn't
-- become a second artifact store. Written by the server on job completion
-- (replace-by-job_run); read from the latest run per pipeline for the project
-- Security tab. fingerprint is computed now for the v2 cross-run dedup.
CREATE TABLE security_findings (
    id            BIGSERIAL PRIMARY KEY,
    job_run_id    UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    run_id        UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pipeline_id   UUID NOT NULL,                 -- denormalized (future rollup); unindexed
    project_id    UUID NOT NULL,                 -- denormalized; unindexed
    job_name      TEXT NOT NULL,
    artifact_id   UUID REFERENCES artifacts(id) ON DELETE SET NULL, -- drill-down to the SARIF blob
    artifact_path TEXT NOT NULL DEFAULT '',      -- survives artifact sweep for display
    tool          TEXT NOT NULL,                 -- SARIF tool.driver.name
    rule_id       TEXT NOT NULL,
    severity      TEXT NOT NULL,                 -- critical|high|medium|low (normalized)
    level         TEXT NOT NULL,                 -- raw SARIF level (error|warning|note|none)
    message       TEXT NOT NULL,
    location_path TEXT NOT NULL DEFAULT '',
    location_line INT NOT NULL DEFAULT 0,
    location_url  TEXT NOT NULL DEFAULT '',
    fingerprint   TEXT NOT NULL,                 -- SARIF fingerprint, else hash(tool|rule|path|line|msg)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- The replace path deletes by job_run; reads start from the latest run per
-- pipeline (run_id), commonly filtered by severity.
CREATE INDEX idx_security_findings_job_run ON security_findings (job_run_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_security_findings_run_severity ON security_findings (run_id, severity);
-- +goose StatementEnd

-- +goose StatementBegin
-- Reconciliation marker: one row per job_run whose SARIF was SUCCESSFULLY parsed
-- (including a clean scan with zero findings). Lets the Security tab distinguish
-- "scanned clean" from "not scanned / parse failed" — the findings list only
-- advances to a newer run once that run has a successful scan here, so a failed
-- or in-flight scan never hides the previous run's known vulnerabilities.
CREATE TABLE security_scans (
    job_run_id    UUID PRIMARY KEY REFERENCES job_runs(id) ON DELETE CASCADE,
    run_id        UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pipeline_id   UUID NOT NULL,
    finding_count INT NOT NULL DEFAULT 0,
    reconciled_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- "latest scanned run per pipeline" reads by pipeline_id (+ runs.counter for
-- ordering).
CREATE INDEX idx_security_scans_pipeline ON security_scans (pipeline_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS security_scans;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS security_findings;
-- +goose StatementEnd
