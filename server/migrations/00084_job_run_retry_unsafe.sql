-- +goose Up
-- +goose StatementBegin

-- retry_unsafe marks a job_run whose job mutates external state — it has a
-- deploy: block OR an environment: (TargetEnvironment() != ""). Such a job
-- must NOT be auto-retried after a disruption (spot preemption / agent loss):
-- a deploy/migration that already applied part of its change would re-run the
-- mutation. Stamped once at job_run creation from the job def (immutable for
-- the row's life), so BOTH the disruption-retry path and the reaper can gate
-- on it with a single cheap column read instead of decoding the run snapshot.
--
-- Default FALSE = the common case (build/test/check jobs, safe to retry).

ALTER TABLE job_runs
    ADD COLUMN retry_unsafe BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose StatementEnd

-- +goose StatementBegin

-- UPGRADE BACKFILL: rows created before this migration all defaulted to
-- FALSE. That is fine for TERMINAL rows (the flag only governs future
-- disruption/reclaim decisions), but a job that is still 'queued'/'running'
-- at upgrade time is LIVE — if it is a deploy/environment job and its agent
-- dies after this migration, the new reaper/handler would read
-- retry_unsafe=FALSE and could auto-retry the very job this feature protects.
-- Backfill those live rows from the run's stored definition (PascalCase JSON,
-- decoded elsewhere via domain.Pipeline). The predicate mirrors
-- domain.Job.TargetEnvironment() != "" exactly: Deploy.Environment OR
-- Environment non-empty. Scoped to non-terminal rows so we never rewrite
-- historical rows.

UPDATE job_runs jr
SET retry_unsafe = TRUE
FROM runs r
WHERE jr.run_id = r.id
  AND jr.status IN ('queued', 'running')
  AND EXISTS (
    SELECT 1
    FROM jsonb_array_elements(
      CASE
        WHEN jsonb_typeof(r.definition->'Jobs') = 'array'
        THEN r.definition->'Jobs'
        ELSE '[]'::jsonb
      END
    ) AS job(def)
    WHERE job.def->>'Name' = jr.name
      AND (
        NULLIF(btrim(job.def->>'Environment'), '') IS NOT NULL
        OR NULLIF(btrim(job.def->'Deploy'->>'Environment'), '') IS NOT NULL
      )
  );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE job_runs
    DROP COLUMN IF EXISTS retry_unsafe;

-- +goose StatementEnd
