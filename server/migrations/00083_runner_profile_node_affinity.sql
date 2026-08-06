-- +goose Up
-- +goose StatementBegin

-- Runner profile gains a SOFT node preference: preferred_node_affinity,
-- mirroring k8s nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution.
-- Unlike node_selector (a hard match), this only BIASES scheduling — it lets a
-- profile prefer a node class (e.g. spot) and fall back to another (on-demand)
-- when the preferred class has no room, instead of failing hard.
--
-- JSONB array of { weight, match_expressions:[{key,operator,values}] } so the
-- shape can evolve (e.g. required affinity, pod affinity) without another
-- migration. NOT NULL DEFAULT '[]' keeps every read path uniform (no NULL
-- handling in the store, agent, or UI); the CHECK guards against shape drift
-- from direct SQL / a buggy handler / a future migration (admin API still
-- validates fields via labels.NewRequirement).

ALTER TABLE runner_profiles
    ADD COLUMN preferred_node_affinity JSONB NOT NULL DEFAULT '[]'::JSONB,
    ADD CONSTRAINT runner_profiles_preferred_node_affinity_is_array
        CHECK (jsonb_typeof(preferred_node_affinity) = 'array');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runner_profiles
    DROP CONSTRAINT IF EXISTS runner_profiles_preferred_node_affinity_is_array,
    DROP COLUMN IF EXISTS preferred_node_affinity;

-- +goose StatementEnd
