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
       p.id                        AS project_id,
       p.trust_same_repo_pr_config AS trust_same_repo_pr_config,
       p.notifications             AS project_notifications
FROM materials m
JOIN pipelines pl ON pl.id = m.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE m.id = $1
FOR SHARE OF p;
