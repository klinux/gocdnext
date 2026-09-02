-- name: ListRunsGlobal :many
-- Cross-project timeline: most recent runs first. Carries the
-- pipeline + project names so list views can link without per-row
-- lookups. All filter params accept the empty string as "no filter"
-- so the same query drives the dashboard widget (no filters) and
-- the /runs page (every filter the UI exposes).
SELECT r.id,
       r.pipeline_id,
       pl.name         AS pipeline_name,
       p.id            AS project_id,
       p.slug          AS project_slug,
       p.name          AS project_name,
       r.counter,
       r.cause,
       r.status,
       -- queue_reason rides along so the runs list can explain a held run
       -- ("Frozen: production") without a per-row lookup — same rationale as
       -- cancel_reason above.
       r.queue_reason,
       r.cancel_reason,
       r.superseded_by,
       r.has_services,
       r.service_names,
       r.created_at,
       r.started_at,
       r.finished_at,
       r.triggered_by
FROM runs r
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects  p  ON p.id  = pl.project_id
WHERE (@status_filter::text = '' OR r.status = @status_filter::text)
  AND (@cause_filter::text = '' OR r.cause = @cause_filter::text)
  AND (@project_slug::text = '' OR p.slug = @project_slug::text)
  AND (@pipeline_filter::text = '' OR pl.name = @pipeline_filter::text)
ORDER BY r.created_at DESC
LIMIT $1 OFFSET @row_offset::bigint;

-- name: CountRunsGlobal :one
-- Paired with ListRunsGlobal so /runs can render "N of M" with the
-- same filter args. Returned as bigint to fit any table; UI only
-- needs int32 but this avoids cast noise.
SELECT COUNT(*)::bigint AS total
FROM runs r
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects  p  ON p.id  = pl.project_id
WHERE (@status_filter::text = '' OR r.status = @status_filter::text)
  AND (@cause_filter::text = '' OR r.cause = @cause_filter::text)
  AND (@project_slug::text = '' OR p.slug = @project_slug::text)
  AND (@pipeline_filter::text = '' OR pl.name = @pipeline_filter::text);

-- name: ListPipelineNames :many
-- Distinct pipeline names across every project, for the /runs
-- pipeline filter dropdown. Names repeat across projects by design
-- (build, deploy, quality...), so DISTINCT keeps the list short.
-- system_managed pipelines (e.g. synthetic `_compliance`) are excluded
-- so internal names don't leak into the user-facing dropdown.
SELECT DISTINCT pl.name
FROM pipelines pl
WHERE NOT pl.system_managed
ORDER BY pl.name;

-- name: ListAgentsWithRunning :many
-- Dashboard + /agents list: every agent with its declared metadata
-- + a count of currently-running job_runs it's been assigned.
-- LEFT JOIN + FILTER gives 0 for idle agents without needing a
-- second roundtrip.
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
GROUP BY a.id
ORDER BY a.name;

-- name: DashboardRunsToday :one
-- Count of runs created today (server-local day boundary via
-- now()::date).
SELECT COUNT(*)::bigint AS total
FROM runs
WHERE created_at >= now()::date;

-- name: DashboardSuccessRate7d :one
-- Terminal runs in the last 7 days, broken down by outcome. The
-- caller computes rate = success / (success + failure). Returns
-- 0 when no terminal runs in the window.
SELECT
  COUNT(*) FILTER (WHERE status = 'success')::bigint  AS successes,
  COUNT(*) FILTER (WHERE status = 'failed')::bigint   AS failures,
  COUNT(*) FILTER (WHERE status = 'canceled')::bigint AS canceled
FROM runs
WHERE finished_at IS NOT NULL
  AND finished_at >= now() - INTERVAL '7 days';

-- name: DashboardP50DurationSec7d :one
-- Median run duration in seconds across the last 7 days. NULL when
-- no finished runs.
SELECT COALESCE(
  percentile_cont(0.5) WITHIN GROUP (
    ORDER BY EXTRACT(epoch FROM (finished_at - started_at))
  ),
  0
)::double precision AS p50_seconds
FROM runs
WHERE finished_at IS NOT NULL
  AND started_at IS NOT NULL
  AND finished_at >= now() - INTERVAL '7 days';

-- name: DashboardQueueDepth :one
-- Active backlog: every queued run across the system, plus queued
-- + running job_runs (the scheduler's work left to do).
SELECT
  COUNT(*) FILTER (WHERE r.status = 'queued')::bigint AS queued_runs,
  COALESCE(SUM(CASE WHEN jr.status IN ('queued','running') THEN 1 ELSE 0 END), 0)::bigint AS pending_jobs
FROM runs r
LEFT JOIN job_runs jr ON jr.run_id = r.id;
