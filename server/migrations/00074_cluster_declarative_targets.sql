-- +goose Up
-- +goose StatementBegin

-- Opt-in for pipeline-declared deploy targets (ADR-0001). A deploy target decides WHERE
-- a deploy lands, so letting repo YAML register one is a privilege question, not a
-- convenience one. The rule this column completes:
--
--   allowed_projects EMPTY            -> declarative allowed. No governance was expressed,
--                                        and any project could already target the cluster.
--   allowed_projects SET              -> declarative DENIED by default. An admin curated
--                                        who may use this cluster; curating the target
--                                        stays with the admin.
--   allowed_projects SET + this=true  -> declarative allowed, still restricted to the
--                                        listed projects.
--
-- Default false is deliberate: it only ever matters on a cluster that already carries an
-- allow-list, where the safe answer is "no" until an admin says otherwise. An open
-- cluster is unaffected by this column.
ALTER TABLE clusters
    ADD COLUMN allow_declarative_targets BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementEnd
