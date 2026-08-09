-- +goose Up
-- +goose StatementBegin
-- Persist the environment timeline size so project environment cards can show
-- deploy counts without one COUNT(*) per card or per history expand.
ALTER TABLE environments
    ADD COLUMN total_deploys BIGINT NOT NULL DEFAULT 0;

UPDATE environments e
SET total_deploys = totals.total
FROM (
    SELECT environment_id, COUNT(*)::bigint AS total
    FROM deployment_revisions
    GROUP BY environment_id
) totals
WHERE e.id = totals.environment_id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE environments
    DROP COLUMN IF EXISTS total_deploys;
-- +goose StatementEnd
