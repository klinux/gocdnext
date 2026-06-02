-- stuck_runs_cyclic_needs.sql
--
-- Health-check: list runs that look stuck because of a cyclic
-- `needs:` snapshot baked in BEFORE v0.4.36's parser-side
-- cycle detection (validateNoCycles, see server/internal/parser/parse.go).
--
-- WHEN TO RUN:
--   - Once after deploying v0.4.36 to sweep the cluster for legacy
--     stuck runs.
--   - Periodically (cron, dashboard widget) if you operate a
--     long-lived cluster with significant pre-v0.4.36 history.
--
-- WHAT IT FINDS:
--   Runs in `queued` OR `running` for > 1 hour where at least one
--   queued job_run's `needs[]` references a SIBLING queued
--   job_run whose `needs[]` references it back — a mutual wait
--   that the runtime gate can never resolve. The `running` arm
--   catches mid-run cycles where an earlier independent stage's
--   job dispatched (so r.status='running') but a later stage's
--   cyclic siblings are stuck queued.
--
-- WHAT IT DOES NOT FIND:
--   - Larger cycles (3+ nodes). Detecting general cycles in SQL
--     requires recursive CTE walking the needs graph; the 2-cycle
--     query below is the cheap-and-frequent case. If this returns
--     nothing but stuck runs remain, run the recursive variant
--     below.
--   - Runs stuck for legitimate reasons (no idle agent matching
--     required tags, approval awaiting decision, serial-busy
--     waiting on a long predecessor). Use `runs.queue_reason` to
--     distinguish those.
--
-- WHAT TO DO WITH RESULTS:
--   For each row, the operator's choices are:
--     1. Cancel the run via the API/UI — cleanest exit.
--     2. Fix the pipeline YAML (re-apply project) so future runs
--        don't carry the cyclic snapshot.
--   The query is read-only. Cancel actions go through the normal
--   `CancelRun` path so the cascade closes stages + cleans up
--   queued descendants properly.

-- ─── Tier 1: explicit 2-cycle (cheap, covers most cases) ───────
SELECT r.id           AS run_id,
       r.pipeline_id,
       r.counter,
       r.created_at,
       j1.name        AS job_a,
       j1.needs       AS job_a_needs,
       j2.name        AS job_b,
       j2.needs       AS job_b_needs,
       NOW() - r.created_at AS queued_for
FROM runs r
JOIN job_runs j1 ON j1.run_id = r.id
JOIN job_runs j2 ON j2.run_id = r.id
                AND j2.name = ANY(j1.needs)
                AND j1.name = ANY(j2.needs)
                AND j2.id <> j1.id
-- Filter includes 'running' as well as 'queued': a run can be
-- in 'running' state with an earlier independent stage's job
-- already dispatched while a later same-stage cycle is still
-- queued. Without 'running' the cycle would be invisible until
-- the running job finishes (and even then only if it's the last
-- non-cyclic one). 'queued' alone misses these mid-run cycles.
WHERE r.status IN ('queued', 'running')
  AND r.created_at < NOW() - INTERVAL '1 hour'
  AND j1.status = 'queued'
  AND j2.status = 'queued'
ORDER BY r.created_at;

-- ─── Tier 2: general cycle (recursive, run if Tier 1 is empty ──
--          but runs remain stuck > 1h with all needs queued) ───
--
-- WITH RECURSIVE deps AS (
--     SELECT id, run_id, name,
--            unnest(needs) AS dep_name,
--            ARRAY[name] AS path
--     FROM job_runs
--     WHERE status = 'queued'
--   UNION ALL
--     SELECT j.id, j.run_id, j.name,
--            unnest(j.needs),
--            d.path || j.name
--     FROM job_runs j
--     JOIN deps d ON d.run_id = j.run_id
--                AND d.dep_name = j.name
--                AND j.status = 'queued'
--                AND NOT (j.name = ANY(d.path))  -- prune visited
-- )
-- SELECT DISTINCT r.id, r.counter, d.path || d.dep_name AS cycle
-- FROM deps d
-- JOIN runs r ON r.id = d.run_id
-- WHERE d.dep_name = ANY(d.path)              -- back-edge to a visited node
--   AND r.status IN ('queued', 'running')
--   AND r.created_at < NOW() - INTERVAL '1 hour'
-- ORDER BY r.id;
--
-- Commented out by default because it's O(N×depth) per run and
-- can hit work_mem on a backlog of stuck runs. Uncomment + run
-- manually if Tier 1 is empty but you have other evidence runs
-- are stuck (UI shows `queued` > N hours, queue_reason is NULL).
