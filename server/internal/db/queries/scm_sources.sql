-- name: UpsertScmSource :one
-- Bind a project to its SCM source. updated_at only bumps when
-- something meaningful changes, so idempotent re-applies don't
-- spam the timeline. webhook_secret is BYTEA ciphertext (sealed
-- in the store layer via crypto.Cipher); sending NULL means
-- "keep the existing ciphertext" so rotation is explicit — a
-- Plain upsert without a secret doesn't wipe an existing one.
INSERT INTO scm_sources (project_id, provider, url, default_branch, webhook_secret, auth_ref)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (project_id) DO UPDATE SET
    provider       = EXCLUDED.provider,
    url            = EXCLUDED.url,
    default_branch = EXCLUDED.default_branch,
    webhook_secret = COALESCE(EXCLUDED.webhook_secret, scm_sources.webhook_secret),
    auth_ref       = EXCLUDED.auth_ref,
    updated_at = CASE
        WHEN scm_sources.provider = EXCLUDED.provider
             AND scm_sources.url = EXCLUDED.url
             AND scm_sources.default_branch = EXCLUDED.default_branch
             AND (EXCLUDED.webhook_secret IS NULL
                  OR scm_sources.webhook_secret IS NOT DISTINCT FROM EXCLUDED.webhook_secret)
             AND scm_sources.auth_ref IS NOT DISTINCT FROM EXCLUDED.auth_ref
        THEN scm_sources.updated_at
        ELSE NOW()
    END
RETURNING id, project_id, provider, url, default_branch, auth_ref,
          last_synced_at, last_synced_revision, created_at, updated_at, (xmax = 0) AS created;

-- name: FindScmSourceByURL :one
-- Read path used by webhook drift detection and future UI
-- listings. Does NOT return webhook_secret — that's handled by
-- GetScmSourceWebhookSecret to keep ciphertext out of the general
-- read path.
SELECT id, project_id, provider, url, default_branch, auth_ref,
       last_synced_at, last_synced_revision, created_at, updated_at
FROM scm_sources
WHERE url = $1
LIMIT 1;

-- name: GetScmSourceByProject :one
SELECT id, project_id, provider, url, default_branch, auth_ref,
       last_synced_at, last_synced_revision, created_at, updated_at
FROM scm_sources
WHERE project_id = $1
LIMIT 1;

-- name: GetScmSourceWebhookSecretByURL :one
-- Webhook-handler path: pulls the sealed secret + the scm_source
-- id for a given clone_url so HandleGitHub can verify HMAC with
-- the right per-repo key. Returns an empty BYTEA when the row
-- has no secret configured yet (the caller then answers 401 —
-- "no webhook secret registered for this repo").
SELECT id, project_id, webhook_secret
FROM scm_sources
WHERE url = $1
LIMIT 1;

-- name: UpdateScmSourceWebhookSecret :exec
-- Rotation path. Takes the newly sealed ciphertext and bumps
-- updated_at. Intended for POST /api/v1/projects/{slug}/scm-sources
-- /{id}/rotate-webhook-secret.
UPDATE scm_sources
SET webhook_secret = $2, updated_at = NOW()
WHERE id = $1;

-- name: UpdateScmSourceSynced :exec
-- Stamp the last successful config sync. Called after a drift re-apply so
-- operators can see whether the live config tracks HEAD.
UPDATE scm_sources
SET last_synced_at = NOW(), last_synced_revision = $2
WHERE id = $1;

-- name: GetProjectByID :one
SELECT id, slug, name, description, config_path, created_at, updated_at
FROM projects
WHERE id = $1
LIMIT 1;
