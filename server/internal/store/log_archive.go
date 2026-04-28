package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/logarchive"
)

// JobLogsForArchive returns every log line for one job_run in
// (seq, at) order — exactly what the archiver streams into the
// gzip writer. Returns logarchive.Line instead of LogLine because
// the archiver's interface is the only consumer; staying in the
// archive vocabulary saves a conversion step at the call site.
func (s *Store) JobLogsForArchive(ctx context.Context, jobRunID uuid.UUID) ([]logarchive.Line, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, stream, at, text
		FROM log_lines
		WHERE job_run_id = $1
		ORDER BY seq ASC, at ASC
	`, jobRunID)
	if err != nil {
		return nil, fmt.Errorf("store: read job logs: %w", err)
	}
	defer rows.Close()
	out := make([]logarchive.Line, 0, 256)
	for rows.Next() {
		var l logarchive.Line
		var at pgtype.Timestamptz
		if err := rows.Scan(&l.Seq, &l.Stream, &at, &l.Text); err != nil {
			return nil, err
		}
		if at.Valid {
			l.At = at.Time
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkJobLogsArchived stamps the archive URI on the job_run and
// timestamps the moment. Wraps the sqlc-generated query.
func (s *Store) MarkJobLogsArchived(ctx context.Context, jobRunID uuid.UUID, uri string) error {
	if err := s.q.MarkJobLogsArchived(ctx, db.MarkJobLogsArchivedParams{
		ID:             pgUUID(jobRunID),
		LogsArchiveUri: &uri,
	}); err != nil {
		return fmt.Errorf("store: mark job logs archived: %w", err)
	}
	return nil
}

// DeleteLogLinesByJob drops every log_line row for the given job.
// Used both by retry path (DeleteLogLinesByJob in sweeper.sql is
// the same query) and by the archiver after a successful upload.
// This wrapper exists so the archiver can hold an interface dep
// on store rather than reaching across packages into db.Queries.
func (s *Store) DeleteLogLinesByJob(ctx context.Context, jobRunID uuid.UUID) error {
	if err := s.q.DeleteLogLinesByJob(ctx, pgUUID(jobRunID)); err != nil {
		return fmt.Errorf("store: delete log lines: %w", err)
	}
	return nil
}

// JobLogArchive describes the archive metadata for a job_run. URI
// is empty when no archive has been recorded.
type JobLogArchive struct {
	URI         string
	HasArchive  bool
}

// GetJobLogArchive returns the archive URI for a job_run. Used by
// the read path to decide whether to fall through to log_lines or
// fetch from the artifact store.
func (s *Store) GetJobLogArchive(ctx context.Context, jobRunID uuid.UUID) (JobLogArchive, error) {
	row, err := s.q.GetJobLogArchive(ctx, pgUUID(jobRunID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobLogArchive{}, nil
		}
		return JobLogArchive{}, fmt.Errorf("store: get job log archive: %w", err)
	}
	if row.LogsArchiveUri == nil || *row.LogsArchiveUri == "" {
		return JobLogArchive{}, nil
	}
	return JobLogArchive{
		URI:        *row.LogsArchiveUri,
		HasArchive: true,
	}, nil
}

// GetProjectLogArchiveFlag returns the per-project log archive
// override. Three states:
//
//	(nil, nil)    — no row / no override; caller falls back to global
//	(*true, nil)  — project opts in
//	(*false, nil) — project opts out
func (s *Store) GetProjectLogArchiveFlag(ctx context.Context, projectID uuid.UUID) (*bool, error) {
	row, err := s.q.GetProjectArchiveFlag(ctx, pgUUID(projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get project archive flag: %w", err)
	}
	return row, nil
}

// GetProjectLogArchiveFlagForJob resolves a job_run all the way back
// to its project's log_archive_enabled override in one query —
// what the archive hook needs at terminal time. Returns nil when
// the chain breaks (job/run/pipeline missing) or the project hasn't
// set the override; treat both as "inherit global".
func (s *Store) GetProjectLogArchiveFlagForJob(ctx context.Context, jobRunID uuid.UUID) (*bool, error) {
	row, err := s.q.GetProjectArchiveFlagForRun(ctx, pgUUID(jobRunID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get project archive flag for run: %w", err)
	}
	return row, nil
}

// readArchive fetches the archive blob through the configured
// LogArchiveSource and parses it back into Lines. Returns nil
// without error when no source is wired — callers fall back to the
// DB read path. The full archive is decoded into memory; for the
// 99% case this is fine (a few hundred KB compressed, maybe a few
// MB uncompressed). A future iteration can stream + filter on the
// fly if jobs with hundreds of MB of logs become common.
func (s *Store) readArchive(ctx context.Context, key string) ([]logarchive.Line, error) {
	if s.logArchiveSrc == nil {
		return nil, nil
	}
	rc, err := s.logArchiveSrc.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("store: open archive %s: %w", key, err)
	}
	defer rc.Close()
	lines, err := logarchive.ReadArchive(rc)
	if err != nil {
		return nil, fmt.Errorf("store: parse archive %s: %w", key, err)
	}
	return lines, nil
}

// archivedTail returns the last `limit` lines of an archived job,
// oldest-first within the returned window — same shape as
// TailLogLinesByJob produces from log_lines.
func (s *Store) archivedTail(ctx context.Context, key string, limit int32) ([]LogLineSummary, error) {
	lines, err := s.readArchive(ctx, key)
	if err != nil {
		return nil, err
	}
	if int(limit) > 0 && len(lines) > int(limit) {
		lines = lines[len(lines)-int(limit):]
	}
	out := make([]LogLineSummary, 0, len(lines))
	for _, l := range lines {
		out = append(out, LogLineSummary{
			Seq: l.Seq, Stream: l.Stream, At: l.At, Text: l.Text,
		})
	}
	return out, nil
}

// archivedAfterSeq returns archived lines with seq strictly greater
// than `cursor`, oldest-first, capped at `limit`. Mirrors
// logLinesAfterSeq's contract for the cursor-poll path.
func (s *Store) archivedAfterSeq(ctx context.Context, key string, cursor int64, limit int64) ([]LogLineSummary, error) {
	lines, err := s.readArchive(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]LogLineSummary, 0, 64)
	for _, l := range lines {
		if l.Seq <= cursor {
			continue
		}
		out = append(out, LogLineSummary{
			Seq: l.Seq, Stream: l.Stream, At: l.At, Text: l.Text,
		})
		if int64(len(out)) >= limit {
			break
		}
	}
	return out, nil
}
