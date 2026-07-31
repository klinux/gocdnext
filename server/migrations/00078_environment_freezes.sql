-- +goose Up
-- +goose StatementBegin

-- environment_freezes is the system-enforced change-freeze on a deployment
-- environment. Approval gates answer WHO may approve, never WHETHER anyone
-- should right now; during a month-end close / holiday / incident the only
-- lever today is telling approvers "don't approve" — social, not systemic.
-- A row here makes it systemic: while `production` is frozen no promotion to
-- it is ADMITTED (approving its gate, dispatching its deploy job, or rolling
-- it back) until a maintainer lifts it. The queue-reason vocabulary already
-- reserved `frozen-deploy` for exactly this (00034).
--
-- Frozen <=> a row exists. There is no `frozen BOOLEAN` column and no
-- soft-delete: unfreeze DELETEs, so the table is precisely the set of
-- currently-frozen environments and every read is an index probe on the PK.
--
-- KEYED BY NAME, NOT BY environments.id, and referencing projects (not
-- environments) — deliberately. `environments` rows are LAZY: they are
-- created at the FIRST dispatch of a job carrying deploy:{environment:X}
-- (UpsertEnvironment). Hanging freeze state off that row would leave the
-- first-ever deploy to `production` with nothing to check, so the one deploy
-- a freeze most needs to stop would slip through. Keying on (project, name)
-- lets a maintainer pre-emptively freeze an environment that does not exist
-- yet, and makes the freeze survive a delete/recreate of the environment
-- (a deleted frozen env leaves an "orphan freeze": intentional — a future
-- deploy to that name stays blocked, and the listing surfaces it so it is
-- still unfreezable).
--
-- Serialization against dispatch/approval/rollback is NOT done here: it is a
-- per-(project, env) pg_advisory_xact_lock (store.ProjectEnvFreezeLockKey)
-- taken by both the freeze/unfreeze tx and the admission paths, which is what
-- makes "once the freeze endpoint returns, nothing new is admitted" true.
CREATE TABLE environment_freezes (
    project_id UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    frozen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Canonical actor, derived server-side from the authenticated user
    -- (email -> name -> id), never a client-supplied string. 320 is the
    -- practical maximum length of an email address; users.email/name are
    -- unbounded TEXT (00007) and OIDC claims are persisted raw, so the bound
    -- stops pathological IdP data from inflating the freeze row, the audit
    -- metadata and the UI.
    frozen_by  TEXT        NOT NULL,
    -- Required: a freeze with no stated reason is an unexplained production
    -- outage waiting to happen when the person who set it is off-shift.
    reason     TEXT        NOT NULL,

    PRIMARY KEY (project_id, name),

    -- btrim in the CHECKs (not just `<> ''`) because the store and the API
    -- both trim before writing: an internal or test caller that bypasses them
    -- must still not be able to persist a whitespace-only name/reason/actor,
    -- which would render as an empty, un-actionable freeze card.
    CONSTRAINT environment_freezes_name_valid
        CHECK (btrim(name) <> '' AND char_length(name) <= 64),
    CONSTRAINT environment_freezes_reason_valid
        CHECK (btrim(reason) <> '' AND char_length(reason) <= 500),
    CONSTRAINT environment_freezes_frozen_by_valid
        CHECK (btrim(frozen_by) <> '' AND char_length(frozen_by) <= 320)
);

COMMENT ON TABLE environment_freezes IS
    'Manual change-freeze on a deploy environment. A row means frozen: no deploy to (project_id, name) is admitted until it is deleted.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS environment_freezes;
-- +goose StatementEnd
