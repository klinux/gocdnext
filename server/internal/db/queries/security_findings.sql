-- Security findings ingested from SARIF artifacts (#71 v1).

-- name: SecurityFindingContext :one
-- Resolve run/pipeline/project/job metadata from the completed job_run, so the
-- ingestion only needs the job_run id. Used to stamp the denormalized columns
-- on each finding (CopyFrom can't join).
SELECT j.run_id, r.pipeline_id, p.project_id, j.name AS job_name
FROM job_runs j
JOIN runs r ON r.id = j.run_id
JOIN pipelines p ON p.id = r.pipeline_id
WHERE j.id = $1;

-- name: DeleteSecurityFindingsByJobRun :exec
DELETE FROM security_findings WHERE job_run_id = $1;

-- name: UpsertSecurityScan :exec
-- Mark a job_run as successfully reconciled (parsed OK, even with zero findings).
-- Written in the same tx as the findings replace, so the marker and the rows
-- are always consistent.
INSERT INTO security_scans (job_run_id, run_id, pipeline_id, finding_count)
VALUES ($1, $2, $3, $4)
ON CONFLICT (job_run_id) DO UPDATE
    SET run_id        = EXCLUDED.run_id,
        pipeline_id   = EXCLUDED.pipeline_id,
        finding_count = EXCLUDED.finding_count,
        reconciled_at = NOW();

-- name: InsertSecurityFindings :copyfrom
INSERT INTO security_findings (
    job_run_id, run_id, pipeline_id, project_id, job_name,
    artifact_id, artifact_path, tool, rule_id, severity, level,
    message, location_path, location_line, location_url, fingerprint
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
);

-- name: FindingsForProject :many
-- Findings from the latest run per pipeline in the project (counter DESC =
-- newest run; uses idx_runs_pipeline_counter), filtered + paginated. Ordered
-- worst-severity first.
WITH latest AS (
    -- Latest reconciled scan per (pipeline, scanner job) — so each scanner
    -- advances independently: a clean Trivy in a new run doesn't hide a Semgrep
    -- finding whose scan is still in-flight / failed in that run.
    SELECT DISTINCT ON (sc.pipeline_id, jr.name) sc.job_run_id AS id
    FROM security_scans sc
    JOIN job_runs jr ON jr.id = sc.job_run_id
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, jr.name, r.counter DESC
)
SELECT f.id, f.pipeline_id, f.run_id, f.job_name, f.tool, f.rule_id,
       f.severity, f.level, f.message, f.location_path, f.location_line,
       f.location_url, f.artifact_id, f.artifact_path, f.created_at
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
WHERE (sqlc.arg(severity)::text = '' OR f.severity = sqlc.arg(severity))
  AND (sqlc.arg(tool)::text = '' OR f.tool = sqlc.arg(tool))
  AND (sqlc.arg(rule)::text = '' OR f.rule_id = sqlc.arg(rule))
ORDER BY
    CASE f.severity
        WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3
    END,
    f.tool, f.rule_id, f.id
LIMIT sqlc.arg(lim)::int OFFSET sqlc.arg(off)::int;

-- name: CountFindingsForProject :one
WITH latest AS (
    -- Latest reconciled scan per (pipeline, scanner job) — so each scanner
    -- advances independently: a clean Trivy in a new run doesn't hide a Semgrep
    -- finding whose scan is still in-flight / failed in that run.
    SELECT DISTINCT ON (sc.pipeline_id, jr.name) sc.job_run_id AS id
    FROM security_scans sc
    JOIN job_runs jr ON jr.id = sc.job_run_id
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, jr.name, r.counter DESC
)
SELECT COUNT(*)::bigint
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
WHERE (sqlc.arg(severity)::text = '' OR f.severity = sqlc.arg(severity))
  AND (sqlc.arg(tool)::text = '' OR f.tool = sqlc.arg(tool))
  AND (sqlc.arg(rule)::text = '' OR f.rule_id = sqlc.arg(rule));

-- name: SeverityCountsForProject :many
-- Per-severity totals across the latest run per pipeline (unfiltered) — the tab
-- header chips.
WITH latest AS (
    -- Latest reconciled scan per (pipeline, scanner job) — so each scanner
    -- advances independently: a clean Trivy in a new run doesn't hide a Semgrep
    -- finding whose scan is still in-flight / failed in that run.
    SELECT DISTINCT ON (sc.pipeline_id, jr.name) sc.job_run_id AS id
    FROM security_scans sc
    JOIN job_runs jr ON jr.id = sc.job_run_id
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, jr.name, r.counter DESC
)
SELECT f.severity, COUNT(*)::bigint AS n
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
GROUP BY f.severity;
