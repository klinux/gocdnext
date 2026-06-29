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
    SELECT DISTINCT ON (r.pipeline_id) r.id
    FROM runs r
    JOIN pipelines p ON p.id = r.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY r.pipeline_id, r.counter DESC
)
SELECT f.id, f.pipeline_id, f.run_id, f.job_name, f.tool, f.rule_id,
       f.severity, f.level, f.message, f.location_path, f.location_line,
       f.location_url, f.artifact_id, f.artifact_path, f.created_at
FROM security_findings f
JOIN latest l ON l.id = f.run_id
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
    SELECT DISTINCT ON (r.pipeline_id) r.id
    FROM runs r
    JOIN pipelines p ON p.id = r.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY r.pipeline_id, r.counter DESC
)
SELECT COUNT(*)::bigint
FROM security_findings f
JOIN latest l ON l.id = f.run_id
WHERE (sqlc.arg(severity)::text = '' OR f.severity = sqlc.arg(severity))
  AND (sqlc.arg(tool)::text = '' OR f.tool = sqlc.arg(tool))
  AND (sqlc.arg(rule)::text = '' OR f.rule_id = sqlc.arg(rule));

-- name: SeverityCountsForProject :many
-- Per-severity totals across the latest run per pipeline (unfiltered) — the tab
-- header chips.
WITH latest AS (
    SELECT DISTINCT ON (r.pipeline_id) r.id
    FROM runs r
    JOIN pipelines p ON p.id = r.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY r.pipeline_id, r.counter DESC
)
SELECT f.severity, COUNT(*)::bigint AS n
FROM security_findings f
JOIN latest l ON l.id = f.run_id
GROUP BY f.severity;
