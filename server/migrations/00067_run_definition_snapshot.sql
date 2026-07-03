-- +goose Up
-- +goose StatementBegin

-- Snapshot the effective pipeline definition onto the run at materialise time
-- (#97 review). The supersede gate-governance graph — which approval gate governs
-- which deploy env — is resolved from the definition (deploy env lives only there,
-- not on job_runs). Reading pipelines.definition LIVE at approve / cascade time
-- drifts: ApplyProject upserts pipelines.definition in place (no versioned history),
-- so a YAML edit between run creation and gate approval would resolve the gate's
-- envs against the NEW shape — writing the wrong gate-pass marker (false block, or
-- worse a stale deploy slipping past the dispatch backstop). Snapshotting here makes
-- the resolution match the rows the run was actually materialised from — the same
-- drift-safety has_services / service_names / ref already get.
ALTER TABLE runs ADD COLUMN definition JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Best-effort backfill for still-active runs so an in-flight run created before this
-- migration resolves against its (current) pipeline rather than an empty def. Only
-- non-terminal runs matter — terminal ones never hit the approve/cascade path.
-- Accepts a small drift window for these pre-existing runs (no snapshot existed);
-- new runs are exact.
UPDATE runs r SET definition = p.definition
  FROM pipelines p
  WHERE p.id = r.pipeline_id AND r.status IN ('queued', 'running');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN IF EXISTS definition;
-- +goose StatementEnd
