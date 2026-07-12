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

// StartNativeDeployInput is the dispatch-time payload for a native deploy takeover
// (ADR-0001, Model A): everything needed to record the revision + watch and flip the
// job to server-managed running, atomically.
type StartNativeDeployInput struct {
	JobRunID      uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
	Version       string
	DeployedBy    string

	ProjectID        uuid.UUID
	SyncMode         string
	Cluster          string
	Application      string
	Namespace        string
	ExpectedRevision string
	DeadlineAt       time.Time
}

// StartNativeDeployResult reports the takeover outcome. Started is false when the job
// wasn't dispatchable (another tick won the race) — the scheduler simply moves on,
// exactly like a lost AssignJob CAS.
type StartNativeDeployResult struct {
	Started    bool
	RevisionID uuid.UUID
	Attempt    int32
}

// StartNativeDeploy performs the invariant-preserving takeover in ONE tx: flip the
// job queued→running (no agent), then create the deployment_revision (in_progress)
// and the deploy_watch (pre-Sync). Committing all three together means the reaper
// never observes a running-no-agent job without its owning watch, and a crash after
// this commit but before Sync leaves a recoverable pre-Sync watch (the watcher
// deadline-fails it). Sync itself is issued by the caller OUTSIDE this tx (external
// I/O must not hold a DB tx/lock).
func (s *Store) StartNativeDeploy(ctx context.Context, in StartNativeDeployInput) (StartNativeDeployResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Flip the job to server-managed running. Same queued+unassigned CAS as AssignJob;
	// a lost race (ErrNoRows) → Started=false, no error.
	job, err := q.StartServerManagedJob(ctx, pgUUID(in.JobRunID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StartNativeDeployResult{Started: false}, nil
		}
		return StartNativeDeployResult{}, fmt.Errorf("store: start server-managed job: %w", err)
	}

	revID, err := q.CreateDeploymentRevision(ctx, db.CreateDeploymentRevisionParams{
		EnvironmentID: pgUUID(in.EnvironmentID),
		RunID:         nullableUUID(in.RunID),
		JobRunID:      nullableUUID(in.JobRunID),
		Attempt:       job.Attempt,
		Version:       in.Version,
		IsRollback:    false,
		DeployedBy:    in.DeployedBy,
	})
	if err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy revision: %w", err)
	}

	// The watch's WHERE EXISTS(in_progress revision) guard is satisfied by the row
	// just inserted above (same tx, same snapshot).
	if _, err := q.CreateDeployWatch(ctx, db.CreateDeployWatchParams{
		DeploymentRevisionID: revID,
		ProjectID:            pgUUID(in.ProjectID),
		SyncMode:             in.SyncMode,
		Cluster:              in.Cluster,
		Application:          in.Application,
		Namespace:            in.Namespace,
		ExpectedRevision:     in.ExpectedRevision,
		DeadlineAt:           pgTimestamptzFromPtr(&in.DeadlineAt),
	}); err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy watch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy commit: %w", err)
	}
	return StartNativeDeployResult{Started: true, RevisionID: fromPgUUID(revID), Attempt: job.Attempt}, nil
}
