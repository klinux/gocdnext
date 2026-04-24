-- name: ListPollableGitMaterials :many
-- Every git material in the system + the project-level scm_source
-- context (provider, url, auth_ref, default_branch) the poller
-- needs to resolve branch HEAD, plus the per-material poll state.
--
-- Effective-interval filtering (material-level poll_interval from
-- JSONB config OR scm_source fallback) happens in Go because a
-- WHERE clause can only see one side at a time and the JSONB-path
-- arithmetic gets ugly. N is bounded by `sum(git_materials per
-- pipeline)`, typically a few dozen, so scanning in-process is
-- fine. LEFT JOIN on scm_sources: a detached project (no binding)
-- simply has NULL columns and the Go layer skips polling it.
SELECT m.id, m.pipeline_id, pl.project_id, m.config,
       ps.last_polled_at, ps.last_head_sha, ps.last_poll_error,
       s.provider, s.url, s.auth_ref, s.default_branch,
       s.poll_interval_ns AS project_poll_interval_ns
FROM materials m
JOIN pipelines pl ON pl.id = m.pipeline_id
LEFT JOIN scm_sources s ON s.project_id = pl.project_id
LEFT JOIN material_poll_state ps ON ps.material_id = m.id
WHERE m.type = 'git';

-- name: UpsertMaterialPollState :exec
-- Records the outcome of one poll. Three cases, same query:
--  - success, unchanged HEAD: last_head_sha unchanged, error NULL
--  - success, new HEAD:       last_head_sha advanced,  error NULL
--  - failure:                 last_head_sha unchanged (callers pass
--                             the prior value), error message set
-- The "unchanged prior value" contract keeps the UI display
-- stable across transient provider blips — operators see the
-- last-known-good SHA alongside the error rather than a blank.
INSERT INTO material_poll_state (material_id, last_polled_at, last_head_sha, last_poll_error)
VALUES ($1, $2, $3, $4)
ON CONFLICT (material_id) DO UPDATE SET
    last_polled_at  = EXCLUDED.last_polled_at,
    last_head_sha   = EXCLUDED.last_head_sha,
    last_poll_error = EXCLUDED.last_poll_error,
    updated_at      = NOW();
