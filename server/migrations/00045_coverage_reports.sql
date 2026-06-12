-- +goose Up
-- Coverage summaries — one row per job_run that declared a
-- coverage_report. Only the SUMMARY lands here (totals + a capped
-- per-package JSONB breakdown); the raw profile/tracefile never
-- crosses the control plane.
--
-- pipeline_id + job_name + matrix_key are denormalized ON PURPOSE:
-- the trend query ("coverage of pipeline X, per job series, over
-- the last N runs") is the whole point of the table, and the
-- (pipeline_id, job_name, matrix_key, created_at DESC) index below
-- answers it straight off the index — no joins through job_runs →
-- runs at read time. OK at any realistic row count: one row per
-- job run, same cardinality class as job_runs itself.

CREATE TABLE coverage_reports (
    id            BIGSERIAL PRIMARY KEY,
    job_run_id    UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    run_id        UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pipeline_id   UUID NOT NULL,
    job_name      TEXT NOT NULL,
    matrix_key    TEXT NOT NULL DEFAULT '',
    format        TEXT NOT NULL,            -- go-cover | lcov | cobertura
    lines_covered BIGINT NOT NULL,
    lines_total   BIGINT NOT NULL,
    packages      JSONB,                    -- [{name, lines_covered, lines_total}], capped agent-side
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One summary per job run; reruns/attempts overwrite via upsert.
CREATE UNIQUE INDEX idx_coverage_job_run ON coverage_reports (job_run_id);

-- Trend query is PER SERIES (DISTINCT job_name/matrix_key + a
-- LATERAL top-N per series — see queries/coverage.sql): the
-- composite serves both legs. The series enumeration walks the
-- (pipeline_id, job_name, matrix_key) prefix, and each LATERAL
-- probe lands on its series' newest rows via the trailing
-- created_at DESC — no per-series rescan of the pipeline's whole
-- history on matrix-heavy pipelines. A plain (pipeline_id,
-- created_at) index would force exactly that rescan.
CREATE INDEX idx_coverage_pipeline_series_at
    ON coverage_reports (pipeline_id, job_name, matrix_key, created_at DESC);

-- Run detail: all coverage rows of one run (run page section).
CREATE INDEX idx_coverage_run ON coverage_reports (run_id);
