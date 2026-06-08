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
    ADD COLUMN tolerations   JSONB NOT NULL DEFAULT '[]'::JSONB,
    -- Shape guards: belt-and-braces against bad data slipping in via
    -- direct SQL, a buggy admin handler, or a future migration that
    -- mishandles the JSONB encoder. Admin API still validates fields
    -- (operator/effect enums etc); these CHECKs only protect against
    -- container-level shape drift.
    ADD CONSTRAINT runner_profiles_node_selector_is_object
        CHECK (jsonb_typeof(node_selector) = 'object'),
    ADD CONSTRAINT runner_profiles_tolerations_is_array
        CHECK (jsonb_typeof(tolerations) = 'array');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runner_profiles
    DROP CONSTRAINT IF EXISTS runner_profiles_node_selector_is_object,
    DROP CONSTRAINT IF EXISTS runner_profiles_tolerations_is_array,
    DROP COLUMN node_selector,
    DROP COLUMN tolerations;

-- +goose StatementEnd
