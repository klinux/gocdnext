-- +goose Up
-- +goose StatementBegin

-- Runner profiles carry two new bundles for the agent → plugin
-- container env-var injection path: `env` (plaintext, observable in
-- the admin UI) for non-secret config (bucket name, region, default
-- registry), and `secrets` (per-value encrypted with the same AES-GCM
-- cipher project secrets use) for credentials. The whole point is to
-- stop forcing operators to repeat AWS keys per-project just so the
-- buildx plugin can hit an S3 layer cache — the profile holds it
-- once, every job that runs there inherits.
--
-- Layout:
--   env     JSONB  → {"BUCKET": "ci-cache", "REGION": "us-east-1"}
--   secrets JSONB  → {"AWS_ACCESS_KEY_ID": "<hex_ciphertext>", ...}
--
-- Encrypted as hex strings (not raw bytea) so JSONB stays JSON; on
-- decrypt the store hex-decodes back to bytes before AEAD Open.
ALTER TABLE runner_profiles
    ADD COLUMN env     JSONB NOT NULL DEFAULT '{}'::JSONB,
    ADD COLUMN secrets JSONB NOT NULL DEFAULT '{}'::JSONB;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runner_profiles
    DROP COLUMN secrets,
    DROP COLUMN env;

-- +goose StatementEnd
