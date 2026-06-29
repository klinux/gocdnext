-- Compliance posture rollup for the analytics epic (#107 phase 4). Current
-- state (which projects are bound to which frameworks), grouped by a project
-- label — no time window, no environment. Low cardinality (projects × a handful
-- of frameworks), so a live aggregation; no materialized rollup needed.

-- name: ComplianceGroupTotals :many
-- Distinct projects per label-value group — the coverage denominator.
SELECT pl.value AS grp,
       COUNT(DISTINCT pl.project_id)::bigint AS projects_total
FROM project_labels pl
WHERE pl.key = sqlc.arg(label_key)
GROUP BY pl.value
ORDER BY pl.value;

-- name: ComplianceCoverageByFramework :many
-- Per (label-value group, framework): how many of the group's projects are bound
-- to that framework. Only frameworks with at least one bound project in the
-- group appear; the store pairs these with the group totals for the percentage.
SELECT pl.value AS grp,
       f.name AS framework,
       COUNT(DISTINCT pf.project_id)::bigint AS covered
FROM project_labels pl
JOIN project_frameworks pf ON pf.project_id = pl.project_id
JOIN compliance_frameworks f ON f.id = pf.framework_id
WHERE pl.key = sqlc.arg(label_key)
GROUP BY pl.value, f.name
ORDER BY pl.value, f.name;
