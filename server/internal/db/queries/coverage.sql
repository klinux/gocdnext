-- name: UpsertCoverageReport :exec
-- Resolves run/pipeline/job metadata from the job_run row itself so
-- the gRPC handler only needs the job_run id it already validated
-- via snapshot-CAS. Rerun/attempt rewrites land on the same row.
INSERT INTO coverage_reports
    (job_run_id, run_id, pipeline_id, job_name, matrix_key, format, lines_covered, lines_total, packages)
SELECT j.id, j.run_id, r.pipeline_id, j.name, COALESCE(j.matrix_key, ''), $2, $3, $4, $5
FROM job_runs j
JOIN runs r ON r.id = j.run_id
WHERE j.id = $1
ON CONFLICT (job_run_id) DO UPDATE SET
    format        = EXCLUDED.format,
    lines_covered = EXCLUDED.lines_covered,
    lines_total   = EXCLUDED.lines_total,
    packages      = EXCLUDED.packages,
    created_at    = NOW();

-- name: DeleteCoverageReportsByJobRun :exec
-- Clears a job_run's coverage on requeue/rerun so a new attempt never inherits
-- the previous attempt's report. Mirrors the log/test/artifact cleanup those
-- paths already do; coverage was the one job-scoped table they missed.
DELETE FROM coverage_reports WHERE job_run_id = $1;

-- name: CoverageByRun :many
SELECT job_run_id, job_name, matrix_key, format, lines_covered, lines_total, packages, created_at
FROM coverage_reports
WHERE run_id = $1
ORDER BY job_name, matrix_key;

-- name: CoverageTrendByPipeline :many
-- Newest N points PER SERIES (job_name, matrix_key) — a global
-- LIMIT would let one chatty job starve every other sparkline out
-- of the window (review-round MEDIUM). row_number() over the
-- partition keeps each series complete; the per-series cap is the
-- limit arg. Newest-first; caller flips for charting.
SELECT c.run_id, c.job_name, c.matrix_key, c.lines_covered, c.lines_total, c.created_at
FROM (
    SELECT DISTINCT s.job_name, s.matrix_key
    FROM coverage_reports s
    WHERE s.pipeline_id = $1
) series
JOIN LATERAL (
    SELECT run_id, job_name, matrix_key, lines_covered, lines_total, created_at
    FROM coverage_reports cr
    WHERE cr.pipeline_id = $1
      AND cr.job_name = series.job_name
      AND cr.matrix_key = series.matrix_key
    ORDER BY cr.created_at DESC
    LIMIT $2
) c ON TRUE
ORDER BY c.created_at DESC;

-- name: CoverageBaselineByPipeline :many
-- Latest coverage per series from MAINLINE runs, used as the delta
-- baseline. Mainline = branch-head advancement: cause 'webhook'
-- (push deliveries — the store default for branch pushes) and
-- 'poll' (poll-discovered head moves). There is NO 'push' cause in
-- the domain — review round caught the original filter matching
-- nothing real. Tag/PR/manual/cron/upstream runs never baseline.
-- Excludes the asking run so a mainline run compares against its
-- predecessor, not itself. Branch is NOT filtered: pipelines that
-- register multiple push branches mix them — acceptable v1,
-- documented in the YAML reference.
SELECT DISTINCT ON (c.job_name, c.matrix_key)
    c.job_name, c.matrix_key, c.lines_covered, c.lines_total, c.run_id, c.created_at
FROM coverage_reports c
JOIN runs r ON r.id = c.run_id
WHERE c.pipeline_id = $1
  AND c.run_id <> $2
  AND r.cause IN ('webhook', 'poll')
ORDER BY c.job_name, c.matrix_key, c.created_at DESC;
