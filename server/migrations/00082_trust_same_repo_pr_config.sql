-- +goose Up

-- #223: per-project opt-in for running a SAME-REPO pull request against its OWN
-- `.gocdnext/` (the head config) instead of the default-branch definition.
--
-- FALSE by default preserves today's behavior (PR runs the default-branch
-- definition). Enabling it is a deliberate, ADMIN-ONLY, AUDITED decision: the
-- project explicitly accepts that same-repo PR authors control the executable
-- graph for that run — jobs, images, agent profiles (which carry secrets),
-- OIDC id_tokens, clusters, and deploys. Trust is NEVER inferred from repo
-- visibility (public/private); the operator opts in.
--
-- Fork PRs are unaffected by this flag — they always run the default-branch
-- definition (a fork can supply neither its own config nor reach credentials).
ALTER TABLE projects
    ADD COLUMN trust_same_repo_pr_config BOOLEAN NOT NULL DEFAULT false;

-- +goose Down

ALTER TABLE projects DROP COLUMN trust_same_repo_pr_config;
