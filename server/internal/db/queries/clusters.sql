-- name: ListClusters :many
-- Admin UI hot path. Sorted by name so the table reads alphabetical.
-- credential_enc is intentionally NOT selected — the list/detail
-- surface is write-only, the credential never leaves the server.
SELECT id, name, description, auth_type, api_server, ca_cert,
       allowed_projects, created_by, created_at, updated_at
FROM clusters
ORDER BY name;

-- name: GetCluster :one
SELECT id, name, description, auth_type, api_server, ca_cert,
       allowed_projects, created_by, created_at, updated_at
FROM clusters
WHERE id = $1
LIMIT 1;

-- name: GetClusterForDispatch :one
-- Scheduler dispatch path: looks up by name (the stable YAML id) and
-- DOES pull credential_enc — this is the one query allowed to read the
-- sealed credential, decrypted in-process and injected as
-- PLUGIN_KUBECONFIG. allowed_projects re-checked by the caller.
SELECT id, name, auth_type, api_server, ca_cert, credential_enc,
       allowed_projects
FROM clusters
WHERE name = $1
LIMIT 1;

-- name: ClusterExists :one
-- Apply-time existence check for a `cluster:` reference (cheap, no
-- credential). Authorization (allowed_projects) is enforced later at
-- dispatch where the run's project is known.
SELECT EXISTS(SELECT 1 FROM clusters WHERE name = $1);

-- name: GetClusterCredentialEnc :one
-- Used by Update's preserve-sentinel path to re-seal the existing
-- credential when the operator edits a cluster without re-entering it.
SELECT credential_enc FROM clusters WHERE id = $1 LIMIT 1;

-- name: InsertCluster :one
INSERT INTO clusters (
    name, description, auth_type, api_server, ca_cert,
    credential_enc, allowed_projects, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING id, name, description, auth_type, api_server, ca_cert,
          allowed_projects, created_by, created_at, updated_at;

-- name: UpdateCluster :exec
UPDATE clusters
SET name = $2,
    description = $3,
    auth_type = $4,
    api_server = $5,
    ca_cert = $6,
    credential_enc = $7,
    allowed_projects = $8,
    updated_at = NOW()
WHERE id = $1;

-- name: DeleteCluster :exec
-- Caller MUST check no pipeline definition references this cluster
-- name first (the scheduler resolves clusters by name at dispatch and
-- a missing name fails the run loudly).
DELETE FROM clusters WHERE id = $1;
