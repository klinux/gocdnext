-- Org/label security rollup (#71 v3). Counts finding IDENTITIES (not SARIF
-- occurrences) from security_finding_states: an identity is "currently open"
-- when it's present in its scanner's latest reconciled scan
-- (last_seen_run_id = latest_run_id) and state='open'. Grouped by a project
-- label value.

-- name: SecurityRollupGroups :many
-- The group spine: every label value for the key, plus whether any of the
-- group's projects has ever reconciled a scan (clean-vs-never-scanned). Counts
-- are LEFT-merged in Go so a clean/zero group still appears.
SELECT pl.value AS grp,
       EXISTS (
           SELECT 1
           FROM security_scans sc
           JOIN pipelines p ON p.id = sc.pipeline_id
           JOIN project_labels pl2 ON pl2.project_id = p.project_id
           WHERE pl2.key = sqlc.arg(label_key) AND pl2.value = pl.value
       ) AS has_scans
FROM project_labels pl
WHERE pl.key = sqlc.arg(label_key)
GROUP BY pl.value
ORDER BY pl.value;

-- name: SecurityRollupCounts :many
-- Open identities per (label-value group, severity). The latest CTE is the
-- latest reconciled run per (pipeline, scanner_job, matrix_key) across all
-- projects; an identity counts when it's still present in that latest scan and
-- open. Dedupe is intrinsic (one row per identity in security_finding_states).
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key)
        sc.pipeline_id, sc.scanner_job, sc.matrix_key, sc.run_id AS latest_run_id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT pl.value AS grp, sfs.last_severity AS severity, COUNT(*)::bigint AS n
FROM security_finding_states sfs
JOIN latest l
    ON  l.pipeline_id = sfs.pipeline_id
    AND l.scanner_job = sfs.scanner_job
    AND l.matrix_key  = sfs.matrix_key
JOIN project_labels pl ON pl.project_id = sfs.project_id AND pl.key = sqlc.arg(label_key)
WHERE sfs.last_seen_run_id = l.latest_run_id
  AND sfs.state = 'open'
GROUP BY pl.value, sfs.last_severity;

-- name: SecurityRollupAccepted :many
-- Accepted-risk identities per group (shown as a distinct count, never folded
-- into the open severity breakdown). Same present condition as the open counts.
WITH latest AS (
    SELECT DISTINCT ON (sc.pipeline_id, sc.scanner_job, sc.matrix_key)
        sc.pipeline_id, sc.scanner_job, sc.matrix_key, sc.run_id AS latest_run_id
    FROM security_scans sc
    JOIN runs r ON r.id = sc.run_id
    ORDER BY sc.pipeline_id, sc.scanner_job, sc.matrix_key, r.counter DESC
)
SELECT pl.value AS grp, COUNT(*)::bigint AS n
FROM security_finding_states sfs
JOIN latest l
    ON  l.pipeline_id = sfs.pipeline_id
    AND l.scanner_job = sfs.scanner_job
    AND l.matrix_key  = sfs.matrix_key
JOIN project_labels pl ON pl.project_id = sfs.project_id AND pl.key = sqlc.arg(label_key)
WHERE sfs.last_seen_run_id = l.latest_run_id
  AND sfs.state = 'accepted'
GROUP BY pl.value;
