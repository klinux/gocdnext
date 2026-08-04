-- name: LockPRHeadRunContext :one
-- PR-head config (#223). Anchored on the material as the single identity: it
-- derives the material's pipeline and owning project, and takes a FOR SHARE lock
-- on the PROJECT row so a concurrent disable of trust_same_repo_pr_config (or a
-- project/pipeline delete) is linearised against run creation. The caller MUST
-- acquire lockComplianceShared BEFORE this query — lock order is
-- compliance -> project row, matching ApplyProject, to avoid an inverted-order
-- deadlock. Everything the store-side envelope guard needs comes back in one
-- round-trip: the pipeline identity + name + system_managed flag, the project id
-- + trust flag, and the project notifications for inheritance.
SELECT pl.id                       AS pipeline_id,
       pl.name                     AS pipeline_name,
       pl.system_managed           AS system_managed,
       pl.definition_raw           AS base_definition_raw,
       p.id                        AS project_id,
       p.trust_same_repo_pr_config AS trust_same_repo_pr_config,
       p.notifications             AS project_notifications
FROM materials m
JOIN pipelines pl ON pl.id = m.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE m.id = $1
FOR SHARE OF p;

-- name: FindScmSourcesByURL :many
-- All scm_sources bound to a clone URL, with the owning project's PR-head toggle
-- + config path (one round-trip for the wiring's source decision). PR-head
-- config requires EXACTLY ONE binding; 0 or >1 must fail closed rather than
-- silently pick a LIMIT-1 winner — url is NOT unique (00002_scm_sources.sql:26).
SELECT s.id, s.project_id, s.provider, s.url, s.default_branch, s.auth_ref,
       p.trust_same_repo_pr_config, p.config_path
FROM scm_sources s
JOIN projects p ON p.id = s.project_id
WHERE s.url = $1;

-- name: ListMaterialPipelineIdentities :many
-- For the matched PR materials, resolve each to its pipeline identity + owning
-- project, so the wiring can partition system_managed (base flow) from repo
-- pipelines (head flow) and build the authorized set. Read-only, pre-tx.
SELECT m.id AS material_id, pl.id AS pipeline_id, pl.name AS pipeline_name,
       pl.system_managed, pl.project_id
FROM materials m
JOIN pipelines pl ON pl.id = m.pipeline_id
WHERE m.id = ANY(@material_ids::uuid[]);
