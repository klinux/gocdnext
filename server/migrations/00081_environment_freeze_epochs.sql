-- +goose Up
-- +goose StatementBegin

-- environment_freeze_epochs records, per (project, environment), the instant a
-- freeze was most recently LIFTED. It is the FLOOR the approval expirer (#208)
-- adds to a gate's expiry clock so that lifting a change-freeze grants a fresh
-- window instead of instantly cancelling every gate the freeze was holding.
--
-- Why a floor row and not re-stamping job_runs.awaiting_since on unfreeze:
-- re-stamping is fragile under batching and commit-timing (a freeze can cover
-- many parked gates across many runs, and an unfreeze would have to find and
-- rewrite them all atomically). A single per-(project, env) row is written once
-- per unfreeze; the expirer reads MAX(last_unfrozen_at) over the gate's
-- GovernedFreezeEnvs and takes effective_start = max(awaiting_since, that MAX).
-- MAX over no rows (a gate whose envs were never frozen) is NULL and falls back
-- to awaiting_since — behaviour is identical to pre-#208 for the un-frozen case.
--
-- Written ONLY when a freeze row was actually removed (an idempotent unfreeze
-- that deletes nothing does NOT renew the window), inside UnfreezeEnvironment's
-- existing tx, under the same per-(project, env) pg_advisory_xact_lock
-- (store.ProjectEnvFreezeLockKey) that serializes freeze/unfreeze against the
-- admission + expiry paths — so the floor a racing expiry reads is consistent
-- with the freeze state it checks under the same lock.
--
-- Keyed by (project_id, environment) like environment_freezes, and referencing
-- projects (not the lazy environments row) for the same reason: the floor must
-- exist for an environment that has never been deployed to. Rows are retained
-- as history (quota/prune/epoch-GC is tracked separately, issue #213); the row
-- count is bounded by distinct (project, env) pairs ever frozen, and every read
-- is an index probe on the PK.
CREATE TABLE environment_freeze_epochs (
    project_id       UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environment      TEXT        NOT NULL,
    -- clock_timestamp() (wall clock at the statement), NOT now()/transaction
    -- start: the expirer compares this against clock_timestamp() taken after it
    -- has acquired its locks, so both ends must be real-time, not tx-time.
    last_unfrozen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (project_id, environment)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE environment_freeze_epochs;

-- +goose StatementEnd
