-- +goose Up
-- +goose StatementBegin

-- Per-case test results parsed out of JUnit/xUnit XML reports
-- emitted by the job (go test -json via converter, pytest-junit,
-- jest, etc.). One row per test case; the agent parses the XML
-- client-side and ships a TestResultBatch alongside the regular
-- JobResult so the server never has to know the XML format.
--
-- Cases link to job_runs with ON DELETE CASCADE — when an agent
-- retry replaces a job_run, the old test_results get wiped with
-- the row that owns them. That keeps the "tests for this run"
-- query honest (you see the latest attempt, not a merge of all).
--
-- classname + name together identify a test across runs (the
-- JUnit convention: classname = `pkg.ClassName`, name = method);
-- the flakiness index is on that pair + status so the Tests tab
-- can answer "has this flaked in the last 14 days?" without a
-- sequential scan.

CREATE TABLE test_results (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_run_id        UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    suite             TEXT NOT NULL,
    classname         TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL,
    status            TEXT NOT NULL,       -- passed | failed | skipped | errored
    duration_ms       BIGINT NOT NULL DEFAULT 0,
    failure_type      TEXT,                -- NULL on pass/skip; e.g. "AssertionError"
    failure_message   TEXT,                -- short reason
    failure_detail    TEXT,                -- stack / diff body; can be big
    system_out        TEXT,                -- captured stdout for the case
    system_err        TEXT,                -- captured stderr
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary listing index: "show me every case for this job run".
-- The Tests tab hits this path per job card on the run detail
-- page; ordering by suite+name gives a stable readable layout.
CREATE INDEX idx_test_results_job_run ON test_results (job_run_id, suite, name);

-- Flakiness index: given a (classname, name) pair, list its
-- outcomes across recent runs. `at DESC` keeps "last N executions"
-- the default access pattern cheap.
CREATE INDEX idx_test_results_case_at
    ON test_results (classname, name, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS test_results;
-- +goose StatementEnd
