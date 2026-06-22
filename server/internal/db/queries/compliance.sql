-- Compliance frameworks: admin-defined labels assigned to projects.

-- name: ListComplianceFrameworks :many
SELECT id, name, description, created_by, created_at, updated_at
FROM compliance_frameworks
ORDER BY name;

-- name: GetComplianceFramework :one
SELECT id, name, description, created_by, created_at, updated_at
FROM compliance_frameworks
WHERE id = $1
LIMIT 1;

-- name: InsertComplianceFramework :one
INSERT INTO compliance_frameworks (name, description, created_by)
VALUES ($1, $2, $3)
RETURNING id, name, description, created_by, created_at, updated_at;

-- name: UpdateComplianceFramework :execrows
UPDATE compliance_frameworks
SET name = $2, description = $3, updated_at = NOW()
WHERE id = $1;

-- name: DeleteComplianceFramework :exec
DELETE FROM compliance_frameworks WHERE id = $1;

-- name: CountFrameworkUsage :one
-- Delete-guard: how many projects carry the framework and how many policies
-- target it. A framework still in use must not be silently dropped.
SELECT
  (SELECT COUNT(*) FROM project_frameworks pf WHERE pf.framework_id = $1) AS project_count,
  (SELECT COUNT(*) FROM policy_frameworks  pl WHERE pl.framework_id = $1) AS policy_count;

-- Project ↔ framework assignment.

-- name: ListFrameworksByProject :many
SELECT f.id, f.name, f.description, f.created_by, f.created_at, f.updated_at
FROM compliance_frameworks f
JOIN project_frameworks pf ON pf.framework_id = f.id
WHERE pf.project_id = $1
ORDER BY f.name;

-- name: DeleteProjectFrameworks :exec
DELETE FROM project_frameworks WHERE project_id = $1;

-- name: InsertProjectFramework :exec
INSERT INTO project_frameworks (project_id, framework_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListProjectIDsByFrameworks :many
-- Recompute fan-out: every project carrying any of the given frameworks.
SELECT DISTINCT project_id
FROM project_frameworks
WHERE framework_id = ANY($1::uuid[]);

-- name: ListAllProjectIDs :many
SELECT id FROM projects;

-- Compliance policies.

-- name: ListCompliancePolicies :many
-- Admin list: metadata only (the compiled config + source YAML are fetched
-- per-policy on Get).
SELECT id, name, description, enabled, mode, priority, applies_to_all,
       position_before, position_after, created_by, created_at, updated_at
FROM compliance_policies
ORDER BY priority, name;

-- name: GetCompliancePolicy :one
SELECT id, name, description, enabled, mode, priority, applies_to_all,
       position_before, position_after, config_yaml, config,
       created_by, created_at, updated_at
FROM compliance_policies
WHERE id = $1
LIMIT 1;

-- name: InsertCompliancePolicy :one
INSERT INTO compliance_policies
  (name, description, enabled, mode, priority, applies_to_all,
   position_before, position_after, config_yaml, config, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, name, description, enabled, mode, priority, applies_to_all,
          position_before, position_after, created_by, created_at, updated_at;

-- name: UpdateCompliancePolicy :execrows
UPDATE compliance_policies
SET name = $2, description = $3, enabled = $4, mode = $5, priority = $6,
    applies_to_all = $7, position_before = $8, position_after = $9,
    config_yaml = $10, config = $11, updated_at = NOW()
WHERE id = $1;

-- name: DeleteCompliancePolicy :exec
DELETE FROM compliance_policies WHERE id = $1;

-- name: DeletePolicyFrameworks :exec
DELETE FROM policy_frameworks WHERE policy_id = $1;

-- name: InsertPolicyFramework :exec
INSERT INTO policy_frameworks (policy_id, framework_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListFrameworkIDsByPolicy :many
SELECT framework_id FROM policy_frameworks WHERE policy_id = $1;

-- name: ListPolicyConfigsExcept :many
-- Used at create/update to detect job/stage name collisions across policies
-- (two policies must not both own `_compliance_scan` — they would produce
-- duplicate job_runs). Pass uuid.Nil on create to compare against all.
SELECT id, name, config FROM compliance_policies WHERE id <> $1;

-- name: ResolvePoliciesForProject :many
-- The apply-time hot path: every enabled policy that applies to a project —
-- global (applies_to_all) or targeting a framework the project carries —
-- with its compiled config + merge metadata, ordered deterministically.
SELECT DISTINCT p.id, p.name, p.mode, p.priority,
       p.position_before, p.position_after, p.config
FROM compliance_policies p
WHERE p.enabled = TRUE
  AND (
    p.applies_to_all = TRUE
    OR EXISTS (
      SELECT 1
      FROM policy_frameworks pf
      JOIN project_frameworks prf ON prf.framework_id = pf.framework_id
      WHERE pf.policy_id = p.id AND prf.project_id = $1
    )
  )
ORDER BY p.priority, p.name;

-- name: ListPipelineRawByProject :many
-- Recompute support: the stored raw (pre-policy) definitions for a project,
-- re-merged with current policies to refresh the effective definition.
SELECT id, definition_raw FROM pipelines WHERE project_id = $1;

-- name: ListPipelinesForPreview :many
-- Admin "preview effective pipeline": both the pre-policy (raw) and post-merge
-- (effective) definitions for every pipeline of a project, server-owned
-- synthetic pipeline first, then repo pipelines by name — a stable order for
-- the preview panel.
SELECT name, system_managed, definition_raw, definition
FROM pipelines
WHERE project_id = $1
ORDER BY system_managed DESC, name;

-- name: ResolvePoliciesByFrameworks :many
-- What-if preview: every enabled policy that WOULD apply to a project carrying
-- the given framework set — global (applies_to_all) or targeting any of them.
-- An empty array yields only global policies (a project with no frameworks).
-- Mirrors ResolvePoliciesForProject but keys off a hypothetical framework set
-- instead of the project's stored assignment, and never reads project_frameworks.
SELECT DISTINCT p.id, p.name, p.mode, p.priority,
       p.position_before, p.position_after, p.config
FROM compliance_policies p
WHERE p.enabled = TRUE
  AND (
    p.applies_to_all = TRUE
    OR EXISTS (
      SELECT 1
      FROM policy_frameworks pf
      WHERE pf.policy_id = p.id AND pf.framework_id = ANY($1::uuid[])
    )
  )
ORDER BY p.priority, p.name;

-- name: ListPipelineKindsByProject :many
-- Governance reconciliation: each pipeline's id/name plus whether it is the
-- server-owned synthetic pipeline (system_managed). Repo pipelines are those
-- with system_managed = false. Ordered by name so the apply path's
-- PipelinesRemoved list stays deterministic.
SELECT id, name, system_managed FROM pipelines WHERE project_id = $1 ORDER BY name;

-- name: UpdatePipelineEffectiveDefinition :execrows
UPDATE pipelines
SET definition = $2,
    definition_version = CASE
        WHEN definition = $2 THEN definition_version
        ELSE definition_version + 1
    END,
    updated_at = CASE WHEN definition = $2 THEN updated_at ELSE NOW() END
WHERE id = $1;
