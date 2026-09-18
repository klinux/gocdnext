-- name: ListRunsGlobalDefault :many
-- Cross-project timeline, most recent first — o hot path. Same shape
-- que ListRunsGlobalSorted (o handler seleciona uma OU outra), sem
-- CASE ORDER BY. Serve o dashboard widget e o /runs sem sort explícito.
-- Query separada é planner-friendly: sem expressão dependente de
-- parâmetro no ORDER BY, um índice `runs(created_at DESC, id)` futuro
-- pode ser usado (com o CASE, o planner não conseguiria).
-- CR klinux (#301): id DESC como tiebreaker final garante ordem total
-- e paginação estável (evita duplicar/pular linhas em OFFSET quando
-- created_at empata).
SELECT r.id,
       r.pipeline_id,
       pl.name         AS pipeline_name,
       p.id            AS project_id,
       p.slug          AS project_slug,
       p.name          AS project_name,
       r.counter,
       r.cause,
       r.status,
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
ORDER BY r.created_at DESC, r.id DESC
LIMIT $1 OFFSET @row_offset::bigint;

-- name: ListRunsGlobalSorted :many
-- Cross-project timeline com sort explícito do usuário. Usado só quando
-- o handler recebe um sort_key na URL — evita taxar o hot path (widget
-- do dashboard + /runs sem sort) com o CASE (o handler roteia pra
-- ListRunsGlobalDefault nesses casos). CR klinux (#301): mantém CASE
-- só aqui, onde faz sentido pagar o custo por sort do usuário.
-- Tiebreakers: created_at DESC (mesmo default) e id DESC (ordem total
-- pra paginação estável em sorts de baixa cardinalidade como status/
-- cause, onde muitos empates são resolvidos pelos tiebreakers).
-- NULLS LAST em started/duration mantém runs nunca-iniciados no fim
-- em ambas as direções.
SELECT r.id,
       r.pipeline_id,
       pl.name         AS pipeline_name,
       p.id            AS project_id,
       p.slug          AS project_slug,
       p.name          AS project_name,
       r.counter,
       r.cause,
       r.status,
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
ORDER BY
  CASE WHEN @sort_key::text = 'started'  AND @sort_dir::text = 'asc'  THEN r.started_at END ASC NULLS LAST,
  CASE WHEN @sort_key::text = 'started'  AND @sort_dir::text = 'desc' THEN r.started_at END DESC NULLS LAST,
  CASE WHEN @sort_key::text = 'duration' AND @sort_dir::text = 'asc'  THEN EXTRACT(EPOCH FROM (COALESCE(r.finished_at, NOW()) - r.started_at)) END ASC NULLS LAST,
  CASE WHEN @sort_key::text = 'duration' AND @sort_dir::text = 'desc' THEN EXTRACT(EPOCH FROM (COALESCE(r.finished_at, NOW()) - r.started_at)) END DESC NULLS LAST,
  CASE WHEN @sort_key::text = 'counter'  AND @sort_dir::text = 'asc'  THEN r.counter END ASC,
  CASE WHEN @sort_key::text = 'counter'  AND @sort_dir::text = 'desc' THEN r.counter END DESC,
  CASE WHEN @sort_key::text = 'pipeline' AND @sort_dir::text = 'asc'  THEN p.slug || '/' || pl.name END ASC,
  CASE WHEN @sort_key::text = 'pipeline' AND @sort_dir::text = 'desc' THEN p.slug || '/' || pl.name END DESC,
  CASE WHEN @sort_key::text = 'status'   AND @sort_dir::text = 'asc'  THEN r.status END ASC,
  CASE WHEN @sort_key::text = 'status'   AND @sort_dir::text = 'desc' THEN r.status END DESC,
  CASE WHEN @sort_key::text = 'cause'    AND @sort_dir::text = 'asc'  THEN r.cause END ASC,
  CASE WHEN @sort_key::text = 'cause'    AND @sort_dir::text = 'desc' THEN r.cause END DESC,
  r.created_at DESC,
  r.id DESC
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
