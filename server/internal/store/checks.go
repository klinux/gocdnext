package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrCheckRunNotFound signals GetGithubCheckRun found no row. Expected
// state for runs that don't produce a check (no App, no install,
// non-GitHub material, manual/upstream cause).
var ErrCheckRunNotFound = errors.New("store: github check run not found")

// checkLockNamespace namespaces the per-run check advisory lock apart
// from every other advisory lock (compliance, etc.) — the (classid,
// objid) two-key space is disjoint from the single-bigint space those use.
const checkLockNamespace int32 = 0x43484B // "CHK"

func runCheckLockKey(runID uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(runID[0:4]))
}

// WithRunCheckLock serializes GitHub check updates for a single run
// across replicas via a SESSION-level Postgres advisory lock. Reopen and
// complete both read the run status then PATCH GitHub; without
// serialization a stale completion can land between a concurrent reopen's
// read and PATCH and hang the check. Holding the lock across the whole
// read+PATCH critical section closes that window. Session-scoped (not a
// transaction) so it can span the external GitHub call without pinning a
// long-running tx; the lock is released explicitly and again when the
// pooled connection is returned.
func (s *Store) WithRunCheckLock(ctx context.Context, runID uuid.UUID, fn func() error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire conn for check lock: %w", err)
	}
	defer conn.Release()
	key := runCheckLockKey(runID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, checkLockNamespace, key); err != nil {
		return fmt.Errorf("store: acquire check lock: %w", err)
	}
	defer func() {
		// Best-effort: a fresh ctx so unlock still runs if the work's ctx
		// expired. Releasing the connection also drops the session lock.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, checkLockNamespace, key)
	}()
	return fn()
}

// GithubCheckRun is the full shape; reporter uses it to issue the
// PATCH against GitHub's API on run completion.
type GithubCheckRun struct {
	RunID          uuid.UUID
	InstallationID int64
	// CheckRunID is nil in commit_status mode — the row exists as the run's
	// GitHub-reporting identity, but no GitHub Check Run was created. A non-nil
	// id means there is a real check to PATCH.
	CheckRunID *int64
	Owner      string
	Repo       string
	HeadSHA    string
	// StatusContext is the commit-status context posted alongside the check
	// run (e.g. ci/gocdnext/<project>/<pipeline>). Persisted so the terminal
	// status update reuses the exact context/identity without re-deriving from
	// a material that may have changed. Empty on links pre-dating the column.
	StatusContext string
	// ReportingMode is the per-run effective mode (both|check_run|commit_status),
	// persisted at create so complete/reopen/security read it back instead of
	// re-deriving from the project's current setting — a mid-run flip can't
	// strand the reporting.
	ReportingMode string
	// Completed is TRUE once the check has been PATCHed to a terminal
	// state. The reporter reads it on a rerun: GitHub won't cleanly
	// reopen a completed check, so a completed link forces a fresh
	// check run instead of reusing the stale one.
	Completed bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertGithubCheckRunInput is the write-side input.
type UpsertGithubCheckRunInput struct {
	RunID          uuid.UUID
	InstallationID int64
	CheckRunID     *int64 // nil in commit_status mode (no GitHub Check Run)
	Owner          string
	Repo           string
	HeadSHA        string
	StatusContext  string
	ReportingMode  string
}

// UpsertGithubCheckRun writes the run→check link. Idempotent across
// retries of the create-check call.
func (s *Store) UpsertGithubCheckRun(ctx context.Context, in UpsertGithubCheckRunInput) error {
	err := s.q.UpsertGithubCheckRun(ctx, db.UpsertGithubCheckRunParams{
		RunID:          pgUUID(in.RunID),
		InstallationID: in.InstallationID,
		CheckRunID:     in.CheckRunID,
		Owner:          in.Owner,
		Repo:           in.Repo,
		HeadSha:        in.HeadSHA,
		StatusContext:  in.StatusContext,
		ReportingMode:  in.ReportingMode,
	})
	if err != nil {
		return fmt.Errorf("store: upsert github check run: %w", err)
	}
	return nil
}

// GetGithubCheckRun returns ErrCheckRunNotFound when no row links
// this run to a check. Callers use that to decide "report nothing"
// vs "patch the existing check".
func (s *Store) GetGithubCheckRun(ctx context.Context, runID uuid.UUID) (GithubCheckRun, error) {
	row, err := s.q.GetGithubCheckRun(ctx, pgUUID(runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return GithubCheckRun{}, ErrCheckRunNotFound
	}
	if err != nil {
		return GithubCheckRun{}, fmt.Errorf("store: get github check run: %w", err)
	}
	return GithubCheckRun{
		RunID:          fromPgUUID(row.RunID),
		InstallationID: row.InstallationID,
		CheckRunID:     row.CheckRunID,
		Owner:          row.Owner,
		Repo:           row.Repo,
		HeadSHA:        row.HeadSha,
		StatusContext:  row.StatusContext,
		ReportingMode:  row.ReportingMode,
		Completed:      row.Completed,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}, nil
}

// MarkGithubCheckRunCompleted flips the run→check link to completed after the
// reporter PATCHes the check to a terminal state. A later rerun reads this to
// recreate the check rather than reuse a completed one (GitHub won't cleanly
// reopen it). Best-effort from the caller's view: a failure just means the
// next rerun reuses instead of recreating — degraded, not broken.
func (s *Store) MarkGithubCheckRunCompleted(ctx context.Context, runID uuid.UUID) error {
	if err := s.q.MarkGithubCheckRunCompleted(ctx, pgUUID(runID)); err != nil {
		return fmt.Errorf("store: mark github check run completed: %w", err)
	}
	return nil
}

// Check reporting modes — how gocdnext surfaces a run's state to GitHub.
//
//	Both         — Check Run + legacy Commit Status (default, current behavior)
//	CheckRun     — only the rich Check Run
//	CommitStatus — only the straight-to-run Commit Status (Woodpecker/GoCD style)
const (
	CheckReportingBoth         = "both"
	CheckReportingCheckRun     = "check_run"
	CheckReportingCommitStatus = "commit_status"
)

// ValidCheckReportingMode reports whether m is one of the three known modes.
// The DB has a CHECK constraint too — this is the fail-fast at the API edge.
func ValidCheckReportingMode(m string) bool {
	switch m {
	case CheckReportingBoth, CheckReportingCheckRun, CheckReportingCommitStatus:
		return true
	default:
		return false
	}
}

// GetProjectCheckReportingBySlug returns the project's GitHub check reporting
// mode. The column is NOT NULL DEFAULT 'both', so an existing project always
// yields a value; a missing project returns ErrProjectNotFound.
func (s *Store) GetProjectCheckReportingBySlug(ctx context.Context, slug string) (string, error) {
	mode, err := s.q.GetProjectCheckReportingBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrProjectNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get project check reporting mode: %w", err)
	}
	return mode, nil
}

// SetProjectCheckReportingBySlug writes the per-project mode. Returns
// ErrProjectNotFound (not an opaque 500) when the slug matches no project.
// Raw Exec so RowsAffected distinguishes "not found" from "no-op update".
func (s *Store) SetProjectCheckReportingBySlug(ctx context.Context, slug, mode string) error {
	// Guard the required-checks invariant at the store (authoritative) layer, not
	// just the HTTP edge: check_run-only stops posting the commit-status contexts
	// the ruleset requires, which would strand every PR. Refuse while any required
	// pipeline is configured.
	if mode == CheckReportingCheckRun {
		rc, err := s.GetProjectRequiredChecks(ctx, slug)
		if err != nil && !errors.Is(err, ErrProjectNotFound) {
			return err
		}
		if rc != nil && len(rc.Pipelines) > 0 {
			return ErrRequiredChecksNeedCommitStatus
		}
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET check_reporting_mode = $2 WHERE slug = $1`,
		slug, mode)
	if err != nil {
		return fmt.Errorf("store: set project check reporting mode: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}
