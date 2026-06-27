-- +goose Up
-- +goose StatementBegin

-- project_labels: free-form key:value labels on a project — the grouping
-- primitive for cross-project views (team:payments, tier:critical, …). A
-- dedicated table (not a JSONB blob on projects) so "group by label" and
-- "every project where key=team" are cheap indexed reads, which the analytics
-- rollup (DORA per team/org) depends on.
--
-- Unique on (project_id, key, value): a project can carry many keys, the same
-- key with different values (rare but allowed — e.g. owner:a, owner:b), but not
-- the exact same pair twice. Cascade on project delete.
CREATE TABLE project_labels (
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    PRIMARY KEY (project_id, key, value)
);

-- Group/filter path: "which projects carry key=value" and "group by (key,value)"
-- across all projects — the analytics rollup's WHERE/GROUP BY.
CREATE INDEX project_labels_key_value_idx ON project_labels (key, value);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_labels;
-- +goose StatementEnd
