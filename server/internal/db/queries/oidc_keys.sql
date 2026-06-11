-- name: GetActiveOIDCKey :one
-- The single signing key. The partial unique index
-- oidc_signing_keys_one_active guarantees at most one row matches.
SELECT id, kid, alg, private_key_enc, public_key_der, created_at
FROM oidc_signing_keys
WHERE retired_at IS NULL AND revoked_at IS NULL;

-- name: InsertOIDCKey :execrows
-- ON CONFLICT DO NOTHING against the one-active partial index:
-- concurrent EnsureActiveOIDCKey calls race here and exactly one
-- wins. Caller re-SELECTs on 0 rows.
INSERT INTO oidc_signing_keys (kid, alg, private_key_enc, public_key_der)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: InsertOIDCKeyReturning :one
-- Rotation-path insert: unlike InsertOIDCKey (ensure path, where
-- losing the ON CONFLICT race is normal), rotation requires the
-- insert to land and needs the row back so the caller can return
-- the new key WITHOUT a post-commit read — a re-SELECT after
-- commit can fail (context canceled, transient error) and make a
-- completed rotation look like a 500.
INSERT INTO oidc_signing_keys (kid, alg, private_key_enc, public_key_der)
VALUES ($1, $2, $3, $4)
RETURNING id, created_at;

-- name: RetireActiveOIDCKey :execrows
-- Graceful rotation: the key stops signing but stays in the JWKS
-- until retired_at + overlap (ListOIDCJWKSKeys cutoff).
UPDATE oidc_signing_keys
SET retired_at = now()
WHERE retired_at IS NULL AND revoked_at IS NULL;

-- name: RevokeActiveOIDCKey :execrows
-- Emergency rotation: key leaves the JWKS immediately. In-flight
-- tokens become unverifiable — that's the kill switch.
UPDATE oidc_signing_keys
SET revoked_at = now()
WHERE retired_at IS NULL AND revoked_at IS NULL;

-- name: ListOIDCJWKSKeys :many
-- Public keys the JWKS endpoint serves: the active key plus any
-- gracefully-retired key still inside the verification overlap
-- (caller passes cutoff = now - tokenTTL - margin). Revoked keys
-- never appear.
SELECT kid, alg, public_key_der, retired_at
FROM oidc_signing_keys
WHERE revoked_at IS NULL
  AND (retired_at IS NULL OR retired_at > $1)
ORDER BY created_at DESC;

-- name: ListOIDCKeysAdmin :many
-- Admin listing — metadata only, never key material beyond the
-- public DER (which is public by definition but still omitted
-- here; the admin UI shows lifecycle, not crypto).
SELECT id, kid, alg, created_at, retired_at, revoked_at
FROM oidc_signing_keys
ORDER BY created_at DESC;
