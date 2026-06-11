-- +goose Up
-- +goose StatementBegin

-- OIDC issuer signing keys (id_tokens feature). The server mints
-- per-job JWTs that cloud providers verify against our JWKS — the
-- private key is the trust root of every workload-identity
-- federation an operator configures, so it lives encrypted at rest
-- (authCipher AES-256-GCM, same as every other secret column) and
-- the PUBLIC half is stored separately so JWKS serving never has
-- to touch the cipher.
CREATE TABLE oidc_signing_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- RFC 7638 JWK thumbprint (base64url SHA-256 of the canonical
    -- public JWK). Doubles as the JWT header `kid` so verifiers
    -- can pick the right key out of the JWKS during rotation.
    kid             TEXT NOT NULL UNIQUE,
    alg             TEXT NOT NULL DEFAULT 'RS256',
    -- PKCS#8 DER, sealed by the server's authCipher.
    private_key_enc BYTEA NOT NULL,
    -- PKIX DER, plaintext on purpose: it's public material and the
    -- JWKS endpoint reads it on a hot-ish path.
    public_key_der  BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Graceful rotation: key stops SIGNING but stays in the JWKS
    -- until retired_at + token TTL + margin, so in-flight tokens
    -- keep verifying.
    retired_at      TIMESTAMPTZ,
    -- Emergency rotation (compromise): key leaves the JWKS
    -- immediately, outstanding tokens become unverifiable — that
    -- is the point.
    revoked_at      TIMESTAMPTZ
);

-- At most ONE active key, enforced by the database. This is the
-- multi-replica boot-race fix: two replicas calling
-- EnsureActiveOIDCKey concurrently both try INSERT ... ON CONFLICT
-- DO NOTHING; exactly one wins, the loser re-reads the winner's
-- row. The invariant holds forever, not just at boot.
CREATE UNIQUE INDEX oidc_signing_keys_one_active
    ON oidc_signing_keys ((true))
    WHERE retired_at IS NULL AND revoked_at IS NULL;

-- +goose StatementEnd
