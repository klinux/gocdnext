-- +goose Up
-- +goose StatementBegin

-- agents.engine snapshots the agent's announced execution engine
-- ("kubernetes" / "docker" / "shell" — see proto RegisterRequest).
-- Lets the run-terminal CleanupRunServices broadcast filter
-- `ListAgentsForRun` to k8s-capable agents only. Without this
-- filter, a Docker/Shell agent that participated in a mixed-engine
-- run would receive the cleanup, return success-with-0-deleted,
-- and mask the fact that a disconnected k8s agent's pods were
-- never reaped.
--
-- Empty default = "unknown / pre-v0.4.35 agent". The Go layer
-- (ListAgentsForRunK8s in store/dispatch.go) treats empty
-- defensively: include the agent in the target set. A rolling
-- upgrade window (old agents not yet redeployed) still gets best-
-- effort cleanup; the empty value is replaced on the next Register.
ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS engine TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN agents.engine IS
    'Announced execution engine — kubernetes/docker/shell. Empty = unknown / legacy. Set on every Register; filters CleanupRunServices target set to k8s-capable agents.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE agents
    DROP COLUMN IF EXISTS engine;
-- +goose StatementEnd
