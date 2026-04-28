-- +goose Up
-- +goose StatementBegin

-- Convert log_lines to a RANGE-partitioned table keyed on `at`. The
-- single-table layout was a Woodpecker-shaped trap: every job's every
-- line wrote into one heap, autovacuum couldn't keep up, and DELETE
-- of old runs blocked production for minutes. Monthly partitions let
-- the retention sweeper DROP PARTITION (constant-time, zero WAL bloat)
-- instead of issuing million-row DELETEs.
--
-- Schema changes from the previous shape:
--   - PK is now (job_run_id, seq, at). PostgreSQL requires the
--     partition key to be a subset of every UNIQUE/PK constraint —
--     keeping `at` in the key is what makes the partitioning legal.
--   - The old BIGSERIAL `id` is gone. It wasn't read anywhere
--     externally; reads address rows by (job_run_id, seq).
--   - The old UNIQUE (job_run_id, seq) is dropped — without `at` in
--     the columns, PG would refuse it on a partitioned table. Dedup
--     in the agent retry path now keys on (job_run_id, seq, at);
--     because the agent caches the original timestamp on every line
--     buffered for retry, the triple is a tighter dedup than the
--     pair was.

CREATE TABLE log_lines_new (
    job_run_id UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    stream     TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    text       TEXT NOT NULL,
    PRIMARY KEY (job_run_id, seq, at)
) PARTITION BY RANGE (at);

CREATE INDEX log_lines_new_job_seq ON log_lines_new (job_run_id, seq);

-- Pre-cutover catch-all (anything written before today goes here, so
-- the data copy from the unpartitioned table never falls through into
-- a missing range) plus the current month and the next 12 months.
-- The retention package's daily tick keeps the future horizon stocked.
DO $$
DECLARE
    cur_month DATE := date_trunc('month', CURRENT_DATE)::date;
    starts DATE;
    ends   DATE;
    pname  TEXT;
    i      INT;
BEGIN
    EXECUTE format(
        'CREATE TABLE log_lines_pre PARTITION OF log_lines_new '
        'FOR VALUES FROM (MINVALUE) TO (%L)',
        cur_month::timestamptz
    );

    FOR i IN 0..12 LOOP
        starts := cur_month + (i || ' months')::interval;
        ends   := cur_month + ((i + 1) || ' months')::interval;
        pname  := format('log_lines_y%sm%s',
                         to_char(starts, 'YYYY'),
                         to_char(starts, 'MM'));
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF log_lines_new '
            'FOR VALUES FROM (%L) TO (%L)',
            pname, starts::timestamptz, ends::timestamptz
        );
    END LOOP;
END $$;

-- Copy data over. DISTINCT ON guards against the (job_run_id, seq)
-- duplicates the old UNIQUE constraint silently rejected via
-- ON CONFLICT — picking the earliest `at` keeps the visible content
-- closest to what the agent originally streamed.
INSERT INTO log_lines_new (job_run_id, seq, stream, at, text)
SELECT DISTINCT ON (job_run_id, seq) job_run_id, seq, stream, at, text
FROM log_lines
ORDER BY job_run_id, seq, at;

DROP TABLE log_lines;
ALTER TABLE log_lines_new RENAME TO log_lines;
ALTER INDEX log_lines_new_job_seq RENAME TO log_lines_job_seq;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

CREATE TABLE log_lines_unpartitioned (
    id         BIGSERIAL PRIMARY KEY,
    job_run_id UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    stream     TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    text       TEXT NOT NULL,
    UNIQUE (job_run_id, seq)
);
CREATE INDEX idx_log_lines_job_seq ON log_lines_unpartitioned(job_run_id, seq);

INSERT INTO log_lines_unpartitioned (job_run_id, seq, stream, at, text)
SELECT DISTINCT ON (job_run_id, seq) job_run_id, seq, stream, at, text
FROM log_lines
ORDER BY job_run_id, seq, at;

DROP TABLE log_lines;
ALTER TABLE log_lines_unpartitioned RENAME TO log_lines;

-- +goose StatementEnd
