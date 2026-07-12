-- +goose Up
-- +goose StatementBegin

-- deploy_targets is the platform-owned "how does this environment deploy?"
-- descriptor for the native deployment provider (ADR-0001). A pipeline's
-- `deploy: { to: <env> }` resolves the environment, then this row says which
-- ArgoCD Application (on which registered cluster) reconciles it and whether
-- gocdnext triggers the sync or only observes.
--
-- Keyed on environment_id: `environments` (00046) is already the canonical
-- identity of the deploy ledger, so a target is 1:1 with an environment (UNIQUE)
-- rather than duplicating (project_id, name) as loose text. Resolution joins
-- environments(project_id, name) -> deploy_targets.
--
-- cluster is an FK to the immutable cluster name; ON DELETE RESTRICT so a cluster
-- referenced by a target can't be deleted out from under it (the store's cluster
-- delete-guard surfaces a friendly error before the FK does). Field non-emptiness
-- is enforced here as defence in depth; the WRITE path additionally validates
-- cluster->project authorization and rejects multi-source Applications (a
-- `spec.sources` the schema can't see), which is why those are not CHECKs.
CREATE TABLE deploy_targets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    environment_id UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    cluster TEXT NOT NULL REFERENCES clusters(name) ON DELETE RESTRICT,
    application TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT 'argocd',
    sync_mode TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (environment_id),
    CONSTRAINT deploy_targets_provider_check
        CHECK (provider IN ('argocd')),
    CONSTRAINT deploy_targets_sync_mode_check
        CHECK (sync_mode IN ('trigger', 'observe')),
    CONSTRAINT deploy_targets_application_nonempty
        CHECK (btrim(application) <> ''),
    CONSTRAINT deploy_targets_namespace_nonempty
        CHECK (btrim(namespace) <> '')
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS deploy_targets;
-- +goose StatementEnd
