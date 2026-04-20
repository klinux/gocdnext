-- name: FindAgentWithRunning :one
-- Same shape as ListAgentsWithRunning but for one row — reused by
-- the /agents/{id} page. Returns ErrNoRows when the UUID is not
-- an existing agent.
SELECT a.id,
       a.name,
       a.version,
       a.os,
       a.arch,
       a.tags,
       a.capacity,
       a.status,
       a.last_seen_at,
       a.registered_at,
       COALESCE(SUM(CASE WHEN jr.status = 'running' THEN 1 ELSE 0 END), 0)::bigint AS running_jobs
FROM agents a
LEFT JOIN job_runs jr ON jr.agent_id = a.id AND jr.status IN ('running', 'queued')
WHERE a.id = $1
GROUP BY a.id;

-- name: ListJobsForAgent :many
-- Recent jobs dispatched to this agent. Joined all the way up to
-- the project so the table can link to the owning run/project
-- without per-row lookups. LIMIT is caller-supplied to avoid
-- pagination complexity for now (UI caps at 100).
SELECT jr.id            AS job_run_id,
       jr.name          AS job_name,
       jr.status        AS job_status,
       jr.started_at,
       jr.finished_at,
       jr.exit_code,
       r.id             AS run_id,
       r.counter        AS run_counter,
       pl.name          AS pipeline_name,
       p.id             AS project_id,
       p.slug           AS project_slug,
       p.name           AS project_name
FROM job_runs jr
JOIN runs      r  ON r.id  = jr.run_id
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects  p  ON p.id  = pl.project_id
WHERE jr.agent_id = $1
ORDER BY jr.started_at DESC NULLS LAST, jr.id DESC
LIMIT $2;
