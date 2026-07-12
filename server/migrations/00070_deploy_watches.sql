-- +goose Up
-- +goose StatementBegin

-- deploy_watches is the live control-loop record for a single in-flight deploy
-- (ADR-0001, Inc.6). One row exists per deployment_revisions row still
-- `in_progress`: the watcher polls the ArgoCD Application until it converges, then
-- writes the terminal status to deployment_revisions and DELETEs this row IN THE
-- SAME TX. So the table is exactly the set of deploys currently being watched — the
-- claim scan needs no status filter, and the ledger (deployment_revisions, read by
-- the UI deployments tab) stays a clean, immutable-ish history instead of being
-- row-churned by every poll tick.
--
-- Unlike the one-shot supersede-effects lease on runs (00066), this is a
-- long-running controller: claimed_at is a HEARTBEAT renewed each tick, and claim_id
-- is a FENCING TOKEN. A watcher that pauses, loses its lease to another replica, and
-- wakes late must NOT be able to renew or terminalize the deploy — so every
-- renew/finalize/delete matches on claim_id, not just deployment_revision_id.
CREATE TABLE deploy_watches (
    -- 1:1 with the in-flight deploy it observes; dies with the revision row.
    deployment_revision_id UUID PRIMARY KEY
        REFERENCES deployment_revisions(id) ON DELETE CASCADE,

    -- denormalized routing so a tick can Observe() without re-joining the ledger.
    -- project_id: ClusterAPIGet authorizes the cluster per project. cluster: FK to
    -- the immutable name, RESTRICT so a cluster with an in-flight watch can't be
    -- deleted out from under it, orphaning a watch that would only fail much later
    -- (the store delete-guard counts active watches for a friendly 409 first).
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    sync_mode   TEXT NOT NULL,
    cluster     TEXT NOT NULL REFERENCES clusters(name) ON DELETE RESTRICT,
    application TEXT NOT NULL,
    namespace   TEXT NOT NULL,

    -- correlation anchor. expected_revision has NO default: the caller writes the
    -- revision it synced to, or '' DELIBERATELY when the target is unpinned. A
    -- silent default could mask a caller bug and green a deploy against the wrong SHA.
    expected_revision TEXT NOT NULL,

    -- watch_started_at: row creation. The durable path creates the row BEFORE
    -- triggering Sync, so a crash between create and Sync is recoverable (repeat the
    -- Sync — idempotent). sync_requested_at: stamped only AFTER Sync actually fired.
    -- Correlation trusts an operationState only if operation.startedAt >=
    -- sync_requested_at AND syncResult.revision == expected_revision — never a stale
    -- operationState.phase from a prior sync.
    watch_started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sync_requested_at TIMESTAMPTZ,

    -- hard convergence timeout: past it the watcher fails the deploy (deadline
    -- exceeded) instead of polling forever.
    deadline_at TIMESTAMPTZ NOT NULL,

    -- Degraded debounce anchor: the first tick Degraded was seen; NULL when healthy.
    -- Fail only if Degraded persists past the debounce window (a rollout is briefly
    -- Degraded mid-progress).
    degraded_since TIMESTAMPTZ,

    -- claim/lease with a fencing token. All three move together (see CHECK): the
    -- watch is either unclaimed (all NULL) or fully claimed (all set).
    claim_id   UUID,
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT deploy_watches_sync_mode_check
        CHECK (sync_mode IN ('trigger', 'observe')),
    CONSTRAINT deploy_watches_cluster_nonempty
        CHECK (btrim(cluster) <> ''),
    CONSTRAINT deploy_watches_application_nonempty
        CHECK (btrim(application) <> ''),
    CONSTRAINT deploy_watches_namespace_nonempty
        CHECK (btrim(namespace) <> ''),
    CONSTRAINT deploy_watches_deadline_after_start
        CHECK (deadline_at > watch_started_at),
    CONSTRAINT deploy_watches_claim_consistent
        CHECK ((claim_id IS NULL AND claimed_by IS NULL AND claimed_at IS NULL)
            OR (claim_id IS NOT NULL AND claimed_by IS NOT NULL AND claimed_at IS NOT NULL))
);

-- Claim/replay scan: unclaimed or lease-expired watches, oldest lease first, then
-- oldest row — so the longest-waiting deploy is claimed first. Partial-free (the
-- table only holds live watches), NULLS FIRST puts never-claimed rows at the front.
CREATE INDEX deploy_watches_claimable_idx ON deploy_watches (claimed_at NULLS FIRST, created_at);

-- cluster FK column: the delete-guard (CountActiveWatchesForCluster) and FK
-- integrity checks both filter by cluster.
CREATE INDEX deploy_watches_cluster_idx ON deploy_watches (cluster);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS deploy_watches;
-- +goose StatementEnd
