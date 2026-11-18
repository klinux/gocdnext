-- +goose Up

-- Durable "this run produced an artifact" fact for the build-time analytics
-- (#107). The build-duration percentiles must NOT depend on an artifact row
-- still existing: the sweeper deletes artifacts after retention (keep-last /
-- expiry), so an EXISTS(artifacts) test would drop builds from 90/365-day
-- windows and change percentiles retroactively. This flag is set once, when the
-- run's first artifact becomes ready, and is never cleared by retention.

-- +goose StatementBegin
ALTER TABLE runs ADD COLUMN produced_artifact BOOLEAN NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose StatementBegin
-- Backfill only from CONFIRMED artifacts: 'ready', or 'deleting' that was once
-- ready (a persisted content_sha256 is the proof MarkArtifactReady set it). A
-- bare 'pending' row is only an upload URL that was handed out — it may never
-- have been confirmed — so it is not evidence the run produced an artifact.
-- Runs whose artifacts were already swept can't be recovered; the flag is
-- durable from here forward.
UPDATE runs r
SET produced_artifact = true
WHERE EXISTS (
        SELECT 1 FROM artifacts a
        WHERE a.run_id = r.id
          AND (a.status = 'ready'
               OR (a.status = 'deleting' AND a.content_sha256 <> ''))
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN produced_artifact;
-- +goose StatementEnd
