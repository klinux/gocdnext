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

-- name: GetProjectArchiveFlagForRun :one
-- Joins runs -> pipelines -> projects so the archive hook can
-- resolve a job_run's project flag in one query.
SELECT p.log_archive_enabled
FROM job_runs j
JOIN runs r ON r.id = j.run_id
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p ON p.id = pl.project_id
WHERE j.id = $1;

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
-- Flips a queued or running job to its terminal state. Idempotent: matches
-- only non-terminal rows. Accepting 'queued' lets the scheduler fail a job
-- at dispatch time (e.g. unresolved secret) without first flipping it to
-- running via AssignJob. Returns stage/run ids so the caller can cascade.
UPDATE job_runs
SET status = $2, exit_code = $3, error = $4, finished_at = NOW()
WHERE id = $1 AND status IN ('queued', 'running')
RETURNING id, run_id, stage_run_id, agent_id, name;

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
