package store

import (
	"context"
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
