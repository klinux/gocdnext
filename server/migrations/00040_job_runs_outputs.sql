-- +goose Up
-- Per-job-run output map — small structured key/value pairs the
-- job produces (next version, image digest, deploy URL, etc.)
-- that downstream jobs reference via `${{ needs.<job>.outputs.<key> }}`
-- substitution at dispatch time. See issue #10 for the design.
--
-- Storage as JSONB on the existing job_runs row rather than a
-- separate `job_run_outputs` table because:
--   1. Outputs are read together (the scheduler resolves all refs
--      of a downstream job in one pass) — JSONB fetches in the
--      same row as everything else CompleteJob already returns.
--   2. Per-key indexing isn't needed today; queries are always
--      "give me all outputs of job X", never "find jobs with
--      output key=foo".
--   3. The output map is bounded (~10s of small keys per job is
--      typical; absolute cap enforced at the agent + server layer
--      around 64KB so a misbehaving plugin can't grow the row
--      into a problem).
--
-- NOT NULL DEFAULT '{}' so existing rows + future runs that don't
-- declare outputs get the empty object, not NULL — keeps the
-- scheduler's lookup path branch-free (`outputs->>key` returns
-- NULL whether the row is `{}` or the key is missing, indistinguishable
-- and that's the right semantics).
ALTER TABLE job_runs
    ADD COLUMN outputs JSONB NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE job_runs DROP COLUMN outputs;
