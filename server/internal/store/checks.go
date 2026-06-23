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
	CheckRunID     int64
	Owner          string
	Repo           string
	HeadSHA        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpsertGithubCheckRunInput is the write-side input.
type UpsertGithubCheckRunInput struct {
	RunID          uuid.UUID
	InstallationID int64
	CheckRunID     int64
	Owner          string
	Repo           string
	HeadSHA        string
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
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}, nil
}
