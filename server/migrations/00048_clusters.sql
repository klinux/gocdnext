-- +goose Up
-- +goose StatementBegin
-- Clusters are admin-registered k8s DEPLOY TARGETS — distinct from
-- runner_profiles (which say WHERE the agent's job pod runs). A
-- pipeline job references one by name (`cluster: prod-gke`); the
-- scheduler resolves it at dispatch and injects the kubeconfig as the
-- PLUGIN_KUBECONFIG env var (masked), so kubectl/helm/kustomize jobs
-- authenticate without a per-pipeline kubeconfig secret pasted in the
-- step (the GoCD antipattern this replaces).
--
-- auth_type:
--   kubeconfig — credential_enc holds the full kubeconfig (encrypted).
--   token      — credential_enc holds a bearer token (encrypted);
--                api_server + ca_cert build a kubeconfig at dispatch.
--   in_cluster — no stored credential; the job pod's mounted SA is
--                used (same-cluster deploys; k8s isolated mode only).
--
-- credential_enc is sealed with the server's authCipher (AES-256-GCM),
-- same as oidc_signing_keys / runner_profile secrets. ca_cert is a
-- public PEM (TLS verify) — NOT encrypted. allowed_projects is a
-- governance allow-list of project ids (as text); empty = any project.
CREATE TABLE clusters (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    auth_type TEXT NOT NULL,
    api_server TEXT NOT NULL DEFAULT '',
    ca_cert BYTEA,
    credential_enc BYTEA,
    allowed_projects TEXT[] NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT clusters_auth_type_check
        CHECK (auth_type IN ('kubeconfig', 'token', 'in_cluster'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE clusters;
-- +goose StatementEnd
