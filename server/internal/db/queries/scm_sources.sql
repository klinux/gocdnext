-- name: UpsertScmSource :one
-- Bind a project to its SCM source. updated_at only bumps when something
-- meaningful changes, so idempotent re-applies don't spam the timeline.
INSERT INTO scm_sources (project_id, provider, url, default_branch, webhook_secret, auth_ref)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (project_id) DO UPDATE SET
    provider       = EXCLUDED.provider,
    url            = EXCLUDED.url,
    default_branch = EXCLUDED.default_branch,
    webhook_secret = EXCLUDED.webhook_secret,
    auth_ref       = EXCLUDED.auth_ref,
    updated_at = CASE
        WHEN scm_sources.provider = EXCLUDED.provider
             AND scm_sources.url = EXCLUDED.url
             AND scm_sources.default_branch = EXCLUDED.default_branch
             AND scm_sources.webhook_secret IS NOT DISTINCT FROM EXCLUDED.webhook_secret
             AND scm_sources.auth_ref IS NOT DISTINCT FROM EXCLUDED.auth_ref
        THEN scm_sources.updated_at
        ELSE NOW()
    END
RETURNING id, project_id, provider, url, default_branch, webhook_secret, auth_ref,
          last_synced_at, last_synced_revision, created_at, updated_at, (xmax = 0) AS created;

-- name: FindScmSourceByURL :one
SELECT id, project_id, provider, url, default_branch, webhook_secret, auth_ref,
       last_synced_at, last_synced_revision, created_at, updated_at
FROM scm_sources
WHERE url = $1
LIMIT 1;

-- name: GetScmSourceByProject :one
SELECT id, project_id, provider, url, default_branch, webhook_secret, auth_ref,
       last_synced_at, last_synced_revision, created_at, updated_at
FROM scm_sources
WHERE project_id = $1
LIMIT 1;

-- name: UpdateScmSourceSynced :exec
-- Stamp the last successful config sync. Called after a drift re-apply so
-- operators can see whether the live config tracks HEAD.
UPDATE scm_sources
SET last_synced_at = NOW(), last_synced_revision = $2
WHERE id = $1;

-- name: GetProjectByID :one
SELECT id, slug, name, description, created_at, updated_at
FROM projects
WHERE id = $1
LIMIT 1;
