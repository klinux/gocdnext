-- name: GetJobLogArchive :one
-- Returns the archive URI + timestamp for one job_run. NULL URI =
-- not archived; reads should fall through to log_lines.
SELECT logs_archive_uri, logs_archived_at
FROM job_runs WHERE id = $1;

-- name: MarkJobLogsArchived :exec
-- Stamps the archive URI on the job_run and timestamps the moment.
-- The DELETE of log_lines for this job is a separate step in the
-- archiver so a failed update doesn't leak rows.
UPDATE job_runs
SET logs_archive_uri = $2, logs_archived_at = NOW()
WHERE id = $1;

-- name: GetProjectArchiveFlag :one
-- Surfaces the per-project log_archive_enabled override (NULL =
-- inherit global). Used by the archiver when resolving effective
-- policy for a job.
SELECT log_archive_enabled
FROM projects WHERE id = $1;

-- name: GetProjectArchiveFlagBySlug :one
-- Surfaces the per-project log_archive_enabled override by slug —
-- what the project-settings UI reads when populating the toggle.
SELECT log_archive_enabled
FROM projects WHERE slug = $1;

-- name: UpdateProjectArchiveFlagBySlug :exec
-- Sets the per-project log_archive_enabled override. NULL value
-- means "inherit global" — explicitly clearing the row by writing
-- a NULL works through the same path.
UPDATE projects
SET log_archive_enabled = $2
WHERE slug = $1;

-- name: GetProjectArchiveFlagForRun :one
-- Joins runs -> pipelines -> projects so the archive hook can
-- resolve a job_run's project flag in one query.
SELECT p.log_archive_enabled
FROM job_runs j
JOIN runs r ON r.id = j.run_id
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE j.id = $1;

-- name: ListJobsNeedingArchive :many
-- Reconciliation #1: terminal job_runs that should have been archived
-- but weren't. The submit-on-terminal hook is best-effort; the queue
-- can drop on saturation, the server can crash mid-flight, the
-- artefact backend can be unreachable. The sweeper picks up the
-- stragglers. Joined to the project's archive flag so the sweeper
-- skips jobs whose project opted out — global=on policy may still
-- have per-project off overrides.
--
-- "Terminal" here = finished_at IS NOT NULL AND status not in ('queued','running').
-- A grace window guards against racing the in-flight submit so the
-- sweeper doesn't spam the queue with jobs already being archived.
SELECT j.id, j.run_id, p.log_archive_enabled
FROM job_runs j
JOIN runs r ON r.id = j.run_id
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE j.logs_archive_uri IS NULL
  AND j.finished_at IS NOT NULL
  AND j.finished_at < NOW() - @grace::INTERVAL
  AND j.status NOT IN ('queued', 'running')
LIMIT @lim::int;

-- name: ListOrphanedArchivedJobs :many
-- Reconciliation #2: jobs whose URI is stamped but log_lines rows
-- still exist. Happens when the archiver's DELETE step fails after
-- the URI update lands. The read path already serves from the
-- archive, so the rows are pure cost — sweep them on a slow tick.
SELECT DISTINCT j.id
FROM job_runs j
WHERE j.logs_archive_uri IS NOT NULL
  AND EXISTS (SELECT 1 FROM log_lines l WHERE l.job_run_id = j.id)
LIMIT @lim::int;

-- name: InsertLogLine :exec
-- Agents send log lines with a per-(job_run_id) monotonic seq plus the
-- timestamp the line was emitted on the runner. The PK is the triple
-- (job_run_id, seq, at) — partitioning the table by `at` ruled out a
-- pure (job_run_id, seq) UNIQUE — so retries dedupe on the same triple.
-- The agent caches the original `at` on every buffered line, which
-- makes the triple a tighter dedup key than the old pair anyway: a
-- reissued line keeps its first-emission timestamp.
INSERT INTO log_lines (job_run_id, seq, stream, at, text)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (job_run_id, seq, at) DO NOTHING;

-- name: CompleteJobRun :one
-- Flips a queued or running job to its terminal state, IF the caller's
-- expected agent_id snapshot still matches what's on the row.
-- Idempotent: matches only non-terminal rows.
--
-- Accepting both 'queued' AND 'running' covers two distinct callers:
--   - handleJobResult (agent gRPC): row was 'running' when AssignJob
--     dispatched it. Passes the calling session's agent UUID as
--     expected_agent_id. A stale result from a revoked agent session
--     whose job was reclaimed (agent_id NULL'd) or redispatched
--     (different agent) will NOT match — `IS NOT DISTINCT FROM` is
--     load-bearing here, both on the NULL-NULL match for the
--     scheduler path AND on the NULL-mismatch reject after reclaim.
--   - scheduler.failJobWithError (dispatch-time fail): row is still
--     'queued' (never got AssignJob'd) with agent_id=NULL. Passes
--     NULL for expected_agent_id; the IS NOT DISTINCT FROM matches.
--
-- Returns stage/run ids so the caller can cascade. ErrNoRows when
-- the predicate doesn't match — caller treats as "another path
-- handled this row already" and drops the message silently.
-- outputs is set on the SAME row update as status so a successful
-- CompleteJob atomically publishes the structured k/v outputs the
-- agent shipped (issue #10). Downstream jobs gated on this row's
-- needs: read both columns in one query, so the substitution path
-- sees the final state — no read-after-write race against
-- dispatch. NULL caller param falls back to '{}' so the legacy
-- non-output completion path stays a one-line change at the call
-- site.
UPDATE job_runs
SET status = $2, exit_code = $3, error = $4, finished_at = NOW(),
    outputs = COALESCE(@outputs::jsonb, '{}'::jsonb)
WHERE id = $1
  AND status IN ('queued', 'running')
  AND agent_id IS NOT DISTINCT FROM @expected_agent_id::uuid
  AND attempt = @expected_attempt::int
RETURNING id, run_id, stage_run_id, agent_id, name, started_at, finished_at;

-- name: GetStageProgress :one
-- Counts jobs still working vs already-failed within a stage — the numbers
-- the caller uses to decide whether to promote the stage. `awaiting_approval`
-- is unfinished too: the gate hasn't decided yet, so the stage can't close.
SELECT
    COUNT(*) FILTER (WHERE status IN ('queued', 'running', 'awaiting_approval'))::BIGINT AS unfinished,
    COUNT(*) FILTER (WHERE status = 'failed')::BIGINT                                    AS failed
FROM job_runs
WHERE stage_run_id = $1;

-- name: CompleteStageRun :exec
UPDATE stage_runs
SET status = $2, finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status IN ('queued', 'running');

-- name: GetRunProgress :one
SELECT
    COUNT(*) FILTER (WHERE status IN ('queued', 'running'))::BIGINT AS unfinished,
    COUNT(*) FILTER (WHERE status = 'failed')::BIGINT               AS failed
FROM stage_runs
WHERE run_id = $1;

-- name: CompleteRun :exec
UPDATE runs
SET status = $2, finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status IN ('queued', 'running');

-- name: SkipJobRun :one
-- Marks a still-queued job as 'skipped' with a terminal finish
-- time so GetStageProgress stops counting it as unfinished. The
-- scheduler calls this for synthetic notification jobs whose
-- `on:` trigger doesn't match the run's user-stage outcome —
-- skipped is the right semantic (the job was never attempted on
-- purpose) vs. canceled (user/system stopped it).
UPDATE job_runs
SET status = 'skipped', finished_at = COALESCE(finished_at, NOW())
WHERE id = $1 AND status = 'queued'
RETURNING id, run_id, stage_run_id, name;

-- name: FailJobRunWithReason :one
-- Marks a still-queued job as `failed` (NOT `skipped`) when the
-- scheduler's needs-satisfaction gate determines it can never
-- run: an upstream is in a non-success terminal state (failed /
-- canceled / skipped / missing-from-run). `failed` is the right
-- semantic for the cascade: GetStageProgress / GetRunUserStageOutcome
-- only count `failed` toward stage + run failure aggregates, and
-- a job we EXPECTED to run that didn't IS a failure from the
-- operator's perspective (different from a notification skip,
-- which is "by design").
--
-- The `error` column carries the human-readable reason
-- ("needs unmet: types-generate: failed") so the UI / API
-- distinguishes a true agent-side failure from a needs-cascade
-- failure. Operator grep is the audit surface.
--
-- Was `SkipJobRunWithReason` setting status='skipped'; renamed
-- + status changed to plug a silent-green hole: a snapshot
-- drift (parser allowed it then, schema changed, manual DB
-- poke) where `needs:` references a non-existent job would
-- otherwise let the run finalize as success despite a job that
-- never ran.
UPDATE job_runs
SET status = 'failed',
    finished_at = COALESCE(finished_at, NOW()),
    error = $2
WHERE id = $1 AND status = 'queued'
RETURNING id, run_id, stage_run_id, name;

-- name: GetRunUserStageOutcome :one
-- Aggregate job outcomes across USER stages only (everything except
-- the synthetic _notifications). The cascade uses this to decide the
-- final run.status when finalizing — notification success/failure
-- must not flip a user run from success to failed or vice versa.
SELECT
  COUNT(CASE WHEN j.status = 'failed'   THEN 1 END)::bigint AS failed,
  COUNT(CASE WHEN j.status = 'canceled' THEN 1 END)::bigint AS canceled
FROM job_runs j
JOIN stage_runs s ON s.id = j.stage_run_id
WHERE j.run_id = $1 AND s.name != '_notifications';

-- name: CancelQueuedStagesInRun :exec
-- When a user stage fails we stop dispatching the rest of the run's user
-- stages. Running work stays untouched; the agent will still report its
-- outcome. The synthetic _notifications stage is preserved on purpose —
-- a pipeline that declared `on: failure` notifications still needs them
-- to fire. The scheduler filters the notification jobs by `on:` when
-- dispatching so only the matching ones actually run.
UPDATE stage_runs
SET status = 'canceled', finished_at = COALESCE(finished_at, NOW())
WHERE run_id = $1
  AND status = 'queued'
  AND name != '_notifications';

-- name: CancelQueuedJobsInRun :exec
-- Pending approval gates in a failed run also get canceled so a
-- rejected deploy doesn't leave a "ready to approve" ghost sitting
-- in the UI with no path forward. Reject is the intended decision
-- path; cancel here only fires on upstream stage failure. Jobs
-- inside the synthetic _notifications stage are preserved so
-- `on: failure` notifications still fire.
UPDATE job_runs j
SET status = 'canceled', finished_at = COALESCE(j.finished_at, NOW())
FROM stage_runs s
WHERE j.run_id = $1
  AND s.id = j.stage_run_id
  AND s.name != '_notifications'
  AND j.status IN ('queued', 'awaiting_approval');
