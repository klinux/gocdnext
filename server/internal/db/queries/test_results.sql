-- name: InsertTestResult :exec
-- One INSERT per case. The agent batches N cases into a single
-- gRPC message; the server handler opens a tx and calls this N
-- times. Simpler than a COPY FROM for a workload that caps at
-- a few thousand cases per run.
INSERT INTO test_results (
    job_run_id, suite, classname, name, status,
    duration_ms, failure_type, failure_message, failure_detail,
    system_out, system_err
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11
);

-- name: DeleteTestResultsByJobRun :exec
-- Called before an agent retry re-ingests a rerun's results, so
-- the UI doesn't show a mix of old and new outcomes. The FK's
-- ON DELETE CASCADE handles the job_run → test_results side;
-- this one targets the "same job_run_id, later agent attempt"
-- case where the row survives but its results shouldn't.
DELETE FROM test_results WHERE job_run_id = $1;

-- name: ListTestResultsByRun :many
-- Returns every case across every job in a run. The Tests tab
-- groups them by job_run_id in-memory — cheaper than JOINing
-- job_runs here because the UI already has the job list from
-- RunDetail.
SELECT id, job_run_id, suite, classname, name, status,
       duration_ms, failure_type, failure_message, failure_detail
FROM test_results
WHERE job_run_id = ANY(@job_run_ids::uuid[])
ORDER BY suite, classname, name;

-- name: ListTestCaseHistory :many
-- Returns the last N executions of a single case across every
-- run of every pipeline. Used by the Tests-tab "history" popup
-- so a dev chasing a flake can see the green/red pattern over
-- time. The (classname, name, created_at DESC) index covers
-- this access pattern directly.
SELECT tr.id, tr.status, tr.duration_ms, tr.created_at,
       tr.failure_message,
       r.id          AS run_id,
       r.counter     AS run_counter,
       pl.name       AS pipeline_name,
       p.slug        AS project_slug
FROM test_results tr
JOIN job_runs jr ON jr.id = tr.job_run_id
JOIN runs r      ON r.id = jr.run_id
JOIN pipelines pl ON pl.id = r.pipeline_id
JOIN projects p  ON p.id = pl.project_id
WHERE tr.classname = $1 AND tr.name = $2
ORDER BY tr.created_at DESC
LIMIT $3;

-- name: CountTestResultsByJobRun :many
-- One aggregate row per job_run covering all statuses — drives
-- the pill on each job card ("42 passed · 1 failed") without
-- pulling the full case list. Empty when the job didn't produce
-- reports.
SELECT job_run_id,
       COUNT(*)::bigint                                                     AS total,
       COUNT(CASE WHEN status = 'passed'  THEN 1 END)::bigint               AS passed,
       COUNT(CASE WHEN status = 'failed'  THEN 1 END)::bigint               AS failed,
       COUNT(CASE WHEN status = 'skipped' THEN 1 END)::bigint               AS skipped,
       COUNT(CASE WHEN status = 'errored' THEN 1 END)::bigint               AS errored,
       SUM(duration_ms)::bigint                                              AS duration_ms
FROM test_results
WHERE job_run_id = ANY(@job_run_ids::uuid[])
GROUP BY job_run_id;
