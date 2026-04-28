-- +goose Up
-- +goose StatementBegin

-- Cold-archive opt-in for log_lines. Once a job hits a terminal
-- status the archiver tarballs+gzips the line stream, ships it to
-- the artifact store, records the URI on the job_run, and drops
-- the rows from the partitioned heap.
--
-- Two dimensions of opt-in:
--   1. Global env GOCDNEXT_LOG_ARCHIVE = auto|on|off.
--   2. Per-project override on the projects table (NULL = inherit).
-- Read path checks logs_archive_uri before hitting log_lines so an
-- archived job stays viewable transparently.
ALTER TABLE job_runs
    ADD COLUMN logs_archive_uri TEXT,
    ADD COLUMN logs_archived_at TIMESTAMPTZ;

ALTER TABLE projects
    ADD COLUMN log_archive_enabled BOOLEAN;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE projects DROP COLUMN log_archive_enabled;
ALTER TABLE job_runs
    DROP COLUMN logs_archived_at,
    DROP COLUMN logs_archive_uri;

-- +goose StatementEnd
