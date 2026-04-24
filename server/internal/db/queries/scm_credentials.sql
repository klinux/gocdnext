-- name: ListSCMCredentials :many
-- Admin UI: list every org-level credential. Ciphertext comes
-- along because admins who can read this table already hold the
-- keys; the decrypt step lives in the store layer.
SELECT id, provider, host, api_base, display_name,
       auth_ref_encrypted, created_by, created_at, updated_at
FROM scm_credentials
ORDER BY provider, host;

-- name: GetSCMCredentialByProviderHost :one
-- Resolver hot path. Nil rows silently fall through to
-- scm_source.auth_ref at the caller.
SELECT id, provider, host, api_base, display_name,
       auth_ref_encrypted, created_by, created_at, updated_at
FROM scm_credentials
WHERE provider = $1 AND host = $2
LIMIT 1;

-- name: UpsertSCMCredential :one
-- Insert-or-rotate. ON CONFLICT flips auth_ref_encrypted + api_base
-- + display_name but preserves id + created_at + created_by so
-- audit trails point to the original provisioning.
INSERT INTO scm_credentials
    (provider, host, api_base, display_name, auth_ref_encrypted, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (provider, host) DO UPDATE SET
    api_base           = EXCLUDED.api_base,
    display_name       = EXCLUDED.display_name,
    auth_ref_encrypted = EXCLUDED.auth_ref_encrypted,
    updated_at         = NOW()
RETURNING id, provider, host, api_base, display_name,
          auth_ref_encrypted, created_by, created_at, updated_at,
          (xmax = 0) AS created;

-- name: DeleteSCMCredential :exec
DELETE FROM scm_credentials WHERE id = $1;
