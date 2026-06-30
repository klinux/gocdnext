-- Security findings ingested from SARIF artifacts (#71 v1) + cross-run identity
-- and state (#71 v2).

-- name: SecurityFindingContext :one
-- Resolve run/pipeline/project/job metadata from the completed job_run, so the
-- ingestion only needs the job_run id. Used to stamp the denormalized columns
-- on each finding + the scan marker + the identity (CopyFrom can't join).
SELECT j.run_id, r.pipeline_id, p.project_id, j.name AS job_name,
       COALESCE(j.matrix_key, '')::text AS matrix_key
FROM job_runs j
JOIN runs r ON r.id = j.run_id
JOIN pipelines p ON p.id = r.pipeline_id
WHERE j.id = $1;

-- name: DeleteSecurityFindingsByJobRun :exec
DELETE FROM security_findings WHERE job_run_id = $1;

-- name: UpsertSecurityScan :exec
-- Mark a job_run as successfully reconciled (parsed OK, even with zero findings).
-- Written in the same tx as the findings replace, so the marker and the rows
-- are always consistent. scanner_job/matrix_key denormalize the scanner grain so
-- the latest-scan CTE never joins job_runs.
INSERT INTO security_scans (job_run_id, run_id, pipeline_id, scanner_job, matrix_key, finding_count)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (job_run_id) DO UPDATE
    SET run_id        = EXCLUDED.run_id,
        pipeline_id   = EXCLUDED.pipeline_id,
        scanner_job   = EXCLUDED.scanner_job,
        matrix_key    = EXCLUDED.matrix_key,
        finding_count = EXCLUDED.finding_count,
        reconciled_at = NOW();

-- name: InsertSecurityFindings :copyfrom
INSERT INTO security_findings (
    job_run_id, run_id, pipeline_id, project_id, job_name, matrix_key,
    artifact_id, artifact_path, tool, rule_id, severity, level,
    message, location_path, location_line, location_url, fingerprint
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
);

-- NOTE: the batch identity upsert lives as a raw tx.Exec in the store
-- (ReplaceSecurityFindings) — sqlc's static analyzer can't model the multi-array
-- unnest(a,b,...) FROM-form (valid at runtime). See upsertFindingIdentitiesSQL.

-- name: FindingsForProject :many
-- Findings from the latest reconciled scan per (pipeline, scanner_job, matrix
-- cell) in the project, filtered + paginated, worst-severity first. The identity
-- join surfaces new (first seen in this run) vs existing.
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key) sc.job_run_id AS id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT f.id, f.pipeline_id, f.run_id, f.job_name, f.tool, f.rule_id,
       f.severity, f.level, f.message, f.location_path, f.location_line,
       f.location_url, f.artifact_id, f.artifact_path, f.created_at,
       (CASE WHEN sfs.first_seen_run_id = f.run_id THEN 'new' ELSE 'existing' END)::text AS status,
       COALESCE(sfs.id, 0)::bigint AS state_id,
       COALESCE(sfs.state, 'open')::text AS state,
       COALESCE(sfs.state_reason, '')::text AS state_reason
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
LEFT JOIN security_finding_states sfs
    ON  sfs.pipeline_id = f.pipeline_id
    AND sfs.scanner_job = f.job_name
    AND sfs.matrix_key  = f.matrix_key
    AND sfs.tool        = f.tool
    AND sfs.fingerprint = f.fingerprint
WHERE (sqlc.arg(severity)::text = '' OR f.severity = sqlc.arg(severity))
  AND (sqlc.arg(tool)::text = '' OR f.tool = sqlc.arg(tool))
  AND (sqlc.arg(rule)::text = '' OR f.rule_id = sqlc.arg(rule))
  -- default hides dismissed/false_positive; open + accepted stay visible.
  AND (sqlc.arg(include_resolved)::bool OR COALESCE(sfs.state, 'open') IN ('open', 'accepted'))
ORDER BY
    CASE f.severity
        WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3
    END,
    f.tool, f.rule_id, f.id
LIMIT sqlc.arg(lim)::int OFFSET sqlc.arg(off)::int;

-- name: CountFindingsForProject :one
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key) sc.job_run_id AS id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT COUNT(*)::bigint
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
LEFT JOIN security_finding_states sfs
    ON  sfs.pipeline_id = f.pipeline_id
    AND sfs.scanner_job = f.job_name
    AND sfs.matrix_key  = f.matrix_key
    AND sfs.tool        = f.tool
    AND sfs.fingerprint = f.fingerprint
WHERE (sqlc.arg(severity)::text = '' OR f.severity = sqlc.arg(severity))
  AND (sqlc.arg(tool)::text = '' OR f.tool = sqlc.arg(tool))
  AND (sqlc.arg(rule)::text = '' OR f.rule_id = sqlc.arg(rule))
  AND (sqlc.arg(include_resolved)::bool OR COALESCE(sfs.state, 'open') IN ('open', 'accepted'));

-- name: SeverityCountsForProject :many
-- Per-severity totals across the latest scan per scanner for OPEN findings only
-- — the tab header chips reflect the actionable backlog (accepted risk is shown
-- as its own count, dismissed/false_positive are excluded).
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key) sc.job_run_id AS id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT f.severity, COUNT(*)::bigint AS n
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
LEFT JOIN security_finding_states sfs
    ON  sfs.pipeline_id = f.pipeline_id
    AND sfs.scanner_job = f.job_name
    AND sfs.matrix_key  = f.matrix_key
    AND sfs.tool        = f.tool
    AND sfs.fingerprint = f.fingerprint
WHERE COALESCE(sfs.state, 'open') = 'open'
GROUP BY f.severity;

-- name: AcceptedCountForProject :one
-- Count of accepted-risk findings in the latest scan per scanner — shown as a
-- distinct chip so an acknowledged risk stays visible without inflating the
-- open severity backlog.
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key) sc.job_run_id AS id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT COUNT(*)::bigint
FROM security_findings f
JOIN latest l ON l.id = f.job_run_id
JOIN security_finding_states sfs
    ON  sfs.pipeline_id = f.pipeline_id
    AND sfs.scanner_job = f.job_name
    AND sfs.matrix_key  = f.matrix_key
    AND sfs.tool        = f.tool
    AND sfs.fingerprint = f.fingerprint
WHERE sfs.state = 'accepted';

-- name: FixedFindingsForProject :many
-- Identities for a scanner that has a latest scan, but were NOT seen in that
-- latest run — i.e. fixed/gone since the previous scan. Rendered from the
-- snapshot (the security_findings row is gone). Grain stays (pipeline,
-- scanner_job, matrix_key) — NOT tool — so a tool that stopped emitting (job
-- dropped Semgrep, kept Trivy) correctly retires its old identities. Excludes
-- dismissed/false_positive so resolved noise isn't resurrected as "fixed".
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key)
        sc.pipeline_id, sc.scanner_job, sc.matrix_key, sc.run_id AS latest_run_id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT sfs.id, sfs.pipeline_id, sfs.scanner_job, sfs.matrix_key, sfs.tool,
       sfs.fingerprint, sfs.last_rule_id, sfs.last_severity, sfs.last_level,
       sfs.last_message, sfs.last_location_path, sfs.last_location_line,
       sfs.last_seen_at
FROM security_finding_states sfs
JOIN latest l
    ON  l.pipeline_id = sfs.pipeline_id
    AND l.scanner_job = sfs.scanner_job
    AND l.matrix_key  = sfs.matrix_key
WHERE sfs.last_seen_run_id IS DISTINCT FROM l.latest_run_id
  AND sfs.state IN ('open', 'accepted')
ORDER BY
    CASE sfs.last_severity
        WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3
    END,
    sfs.tool, sfs.last_rule_id, sfs.id
LIMIT sqlc.arg(lim)::int;

-- name: CountFixedFindingsForProject :one
-- Real total of fixed identities (the list above is capped); the header count
-- must not understate when a removed scanner retires a large prior set.
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key)
        sc.pipeline_id, sc.scanner_job, sc.matrix_key, sc.run_id AS latest_run_id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    JOIN pipelines p ON p.id = sc.pipeline_id
    WHERE p.project_id = sqlc.arg(project_id)
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT COUNT(*)::bigint
FROM security_finding_states sfs
JOIN latest l
    ON  l.pipeline_id = sfs.pipeline_id
    AND l.scanner_job = sfs.scanner_job
    AND l.matrix_key  = sfs.matrix_key
WHERE sfs.last_seen_run_id IS DISTINCT FROM l.latest_run_id
  AND sfs.state IN ('open', 'accepted');

-- name: FindingsByRun :many
-- One run's findings (occurrences) with their identity state. Deduped to
-- identities in the store (worst-severity-wins); kept as occurrences here.
SELECT f.job_name AS scanner_job, f.matrix_key, f.tool, f.fingerprint,
       f.rule_id, f.severity, f.message, f.location_path, f.location_line,
       COALESCE(sfs.state, 'open')::text AS state
FROM security_findings f
LEFT JOIN security_finding_states sfs
    ON  sfs.pipeline_id = f.pipeline_id
    AND sfs.scanner_job = f.job_name
    AND sfs.matrix_key  = f.matrix_key
    AND sfs.tool        = f.tool
    AND sfs.fingerprint = f.fingerprint
WHERE f.run_id = $1;

-- name: RunScanSeries :many
-- The run's reconciled scanner series — from security_scans (NOT findings), so a
-- clean scan still registers. Drives has_scans / delta_available / unbaselined.
SELECT DISTINCT scanner_job, matrix_key
FROM security_scans
WHERE run_id = $1;

-- name: RunBaseContext :one
-- The run's pipeline + (for PR runs) the base branch to diff "new in this change"
-- against. base_ref is empty for non-PR runs (delta not applicable).
SELECT pipeline_id, cause, COALESCE(cause_detail->>'pr_base_ref', '')::text AS base_ref
FROM runs WHERE id = $1;

-- name: SecurityBaseline :many
-- The base-branch baseline for "new in this change", in ONE snapshot: the latest
-- reconciled scan PER (scanner_job, matrix_key) on mainline runs of the base
-- branch, LEFT JOINed to its findings. A clean series returns a row with NULL
-- tool/fingerprint — so it still registers as a comparable series (clean base is
-- a baseline) without a separate query racing a concurrent reconcile.
--
-- Branch match is via runs.revisions JSON (there is no runs.branch column);
-- NOTE: in a multi-material pipeline this qualifies a run when ANY material is on
-- the base branch — branch-scoping, not exact per-material binding (acceptable).
WITH base_latest AS (
    SELECT DISTINCT ON (sc.scanner_job, sc.matrix_key)
        sc.job_run_id, sc.scanner_job, sc.matrix_key
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    WHERE sc.pipeline_id = sqlc.arg(pipeline_id)
      AND r.id <> sqlc.arg(exclude_run)
      AND r.cause IN ('webhook', 'poll')
      AND jsonb_path_exists(
          r.revisions,
          '$.* ? (@.branch == $b)',
          jsonb_build_object('b', sqlc.arg(base_ref)::text)
      )
    ORDER BY sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT bl.scanner_job, bl.matrix_key, f.tool, f.fingerprint
FROM base_latest bl
LEFT JOIN security_findings f ON f.job_run_id = bl.job_run_id;

-- name: SetFindingState :one
-- Update a finding identity's human state (open|dismissed|false_positive|
-- accepted). Scoped to project_id (the handler resolves slug → project) so a
-- maintainer of one project can't mutate another's findings. Returns the id when
-- a row matched; no row → not found / wrong project (handler 404s). The state
-- value is validated by the handler AND the column CHECK.
UPDATE security_finding_states
SET state             = sqlc.arg(state),
    state_reason      = sqlc.arg(state_reason),
    state_actor_id    = sqlc.arg(state_actor_id),
    state_actor_email = sqlc.arg(state_actor_email),
    state_updated_at  = NOW()
WHERE id = sqlc.arg(id) AND project_id = sqlc.arg(project_id)
RETURNING id;
