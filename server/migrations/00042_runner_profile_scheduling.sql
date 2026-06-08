-- +goose Up
-- +goose StatementBegin

-- Runner profile gains Kubernetes scheduling hints: a node_selector
-- map and a tolerations list. The original migration (00026) noted
-- that nodeSelector + friends were intended to land via the JSONB
-- `config` column, but a dedicated typed surface scales better —
-- admin API, sqlc, and the agent engine all benefit from concrete
-- columns rather than stringly-typed digging in a generic map.
--
-- Both columns are JSONB so the shape can evolve (e.g. add
-- TolerationSeconds defaults, weighted node affinity, etc) without
-- another migration. NOT NULL with empty-collection defaults keeps
-- read paths uniform: every row has a value, no NULL handling
-- needed in the agent or UI.

ALTER TABLE runner_profiles
    ADD COLUMN node_selector JSONB NOT NULL DEFAULT '{}'::JSONB,
    ADD COLUMN tolerations   JSONB NOT NULL DEFAULT '[]'::JSONB;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runner_profiles
    DROP COLUMN node_selector,
    DROP COLUMN tolerations;

-- +goose StatementEnd
