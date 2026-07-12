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

// ErrDeployWatchNotFound is returned when no deploy_watch exists for a revision.
var ErrDeployWatchNotFound = errors.New("store: deploy watch not found")

// DeployWatch is the durable control-loop record for one in-flight deploy
// (ADR-0001, Inc.6). ClaimID is the fencing token — uuid.Nil when the watch is
// unclaimed. SyncRequestedAt/DegradedSince/ClaimedAt are nil until set.
type DeployWatch struct {
	DeploymentRevisionID uuid.UUID
	ProjectID            uuid.UUID
	SyncMode             string
	Cluster              string
	Application          string
	Namespace            string
	ExpectedRevision     string
	WatchStartedAt       time.Time
	SyncRequestedAt      *time.Time
	DeadlineAt           time.Time
	DegradedSince        *time.Time
	ClaimID              uuid.UUID
	ClaimedBy            string
	ClaimedAt            *time.Time
	CreatedAt            time.Time
}

// DeployWatchInput is the create-time payload. The watch is created unclaimed and
// pre-Sync (sync_requested_at NULL); ExpectedRevision must be set deliberately
// ("" only when the target is genuinely unpinned).
type DeployWatchInput struct {
	DeploymentRevisionID uuid.UUID
	ProjectID            uuid.UUID
	SyncMode             string
	Cluster              string
	Application          string
	Namespace            string
	ExpectedRevision     string
	DeadlineAt           time.Time
}

func deployWatchFromRow(w db.DeployWatch) DeployWatch {
	return DeployWatch{
		DeploymentRevisionID: fromPgUUID(w.DeploymentRevisionID),
		ProjectID:            fromPgUUID(w.ProjectID),
		SyncMode:             w.SyncMode,
		Cluster:              w.Cluster,
		Application:          w.Application,
		Namespace:            w.Namespace,
		ExpectedRevision:     w.ExpectedRevision,
		WatchStartedAt:       w.WatchStartedAt.Time,
		SyncRequestedAt:      pgTimePtr(w.SyncRequestedAt),
		DeadlineAt:           w.DeadlineAt.Time,
		DegradedSince:        pgTimePtr(w.DegradedSince),
		ClaimID:              fromPgUUID(w.ClaimID),
		ClaimedBy:            stringValue(w.ClaimedBy),
		ClaimedAt:            pgTimePtr(w.ClaimedAt),
		CreatedAt:            w.CreatedAt.Time,
	}
}

// CreateDeployWatch inserts the control-loop record for a fresh in_progress
// deployment revision (unclaimed, pre-Sync).
func (s *Store) CreateDeployWatch(ctx context.Context, in DeployWatchInput) (DeployWatch, error) {
	w, err := s.q.CreateDeployWatch(ctx, db.CreateDeployWatchParams{
		DeploymentRevisionID: pgUUID(in.DeploymentRevisionID),
		ProjectID:            pgUUID(in.ProjectID),
		SyncMode:             in.SyncMode,
		Cluster:              in.Cluster,
		Application:          in.Application,
		Namespace:            in.Namespace,
		ExpectedRevision:     in.ExpectedRevision,
		DeadlineAt:           pgTimestamptzFromPtr(&in.DeadlineAt),
	})
	if err != nil {
		return DeployWatch{}, fmt.Errorf("store: create deploy watch: %w", err)
	}
	return deployWatchFromRow(w), nil
}

// GetDeployWatch fetches the watch for a revision, or ErrDeployWatchNotFound.
func (s *Store) GetDeployWatch(ctx context.Context, revID uuid.UUID) (DeployWatch, error) {
	w, err := s.q.GetDeployWatch(ctx, pgUUID(revID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeployWatch{}, ErrDeployWatchNotFound
		}
		return DeployWatch{}, fmt.Errorf("store: get deploy watch: %w", err)
	}
	return deployWatchFromRow(w), nil
}

// ClaimDeployWatches claims up to max never-claimed-or-lease-expired watches for
// claimedBy, assigning each a fresh fencing token. leaseSeconds is the lease TTL:
// a claim older than it is reclaimable.
func (s *Store) ClaimDeployWatches(ctx context.Context, claimedBy string, leaseSeconds, max int) ([]DeployWatch, error) {
	rows, err := s.q.ClaimDeployWatches(ctx, db.ClaimDeployWatchesParams{
		ClaimedBy:    &claimedBy,
		LeaseSeconds: int32(leaseSeconds),
		MaxBatch:     int32(max),
	})
	if err != nil {
		return nil, fmt.Errorf("store: claim deploy watches: %w", err)
	}
	out := make([]DeployWatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, deployWatchFromRow(r))
	}
	return out, nil
}

// RenewDeployWatch extends the lease under the fencing token. Returns false when the
// lease was lost (0 rows) — the watcher must drop the work.
func (s *Store) RenewDeployWatch(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	n, err := s.q.RenewDeployWatch(ctx, db.RenewDeployWatchParams{
		DeploymentRevisionID: pgUUID(revID), ClaimID: pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: renew deploy watch: %w", err)
	}
	return n > 0, nil
}

// MarkDeployWatchSyncRequested stamps the correlation anchor after Sync fired. Fenced.
func (s *Store) MarkDeployWatchSyncRequested(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	n, err := s.q.MarkDeployWatchSyncRequested(ctx, db.MarkDeployWatchSyncRequestedParams{
		DeploymentRevisionID: pgUUID(revID), ClaimID: pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: mark deploy watch sync-requested: %w", err)
	}
	return n > 0, nil
}

// SetDeployWatchDegradedSince opens the debounce window on the first Degraded tick
// (COALESCE keeps the earliest). Fenced.
func (s *Store) SetDeployWatchDegradedSince(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	n, err := s.q.SetDeployWatchDegradedSince(ctx, db.SetDeployWatchDegradedSinceParams{
		DeploymentRevisionID: pgUUID(revID), ClaimID: pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: set deploy watch degraded-since: %w", err)
	}
	return n > 0, nil
}

// ClearDeployWatchDegraded resets the debounce anchor on health recovery. Fenced.
func (s *Store) ClearDeployWatchDegraded(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	n, err := s.q.ClearDeployWatchDegraded(ctx, db.ClearDeployWatchDegradedParams{
		DeploymentRevisionID: pgUUID(revID), ClaimID: pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: clear deploy watch degraded: %w", err)
	}
	return n > 0, nil
}

// FinalizeDeployWatch atomically terminalizes the deploy: it deletes the watch
// (fenced on claimID) and, in the SAME tx, flips the revision to status. Returns
// false WITHOUT any effect when the lease was lost (the fenced delete matched 0
// rows) — the fencing guarantee: a reclaimed watcher can't terminalize. status is
// "success" or "failed".
func (s *Store) FinalizeDeployWatch(ctx context.Context, revID, claimID uuid.UUID, status string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: finalize deploy watch begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Fenced delete FIRST: 0 rows → lease lost → abort without touching the revision.
	del, err := q.DeleteDeployWatchClaimed(ctx, db.DeleteDeployWatchClaimedParams{
		DeploymentRevisionID: pgUUID(revID), ClaimID: pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: finalize deploy watch delete: %w", err)
	}
	if del == 0 {
		return false, nil
	}
	// Lease held: terminalize the revision (idempotent — a re-delivered finalize on
	// an already-final revision affects 0 rows, but the watch is correctly gone).
	if _, err := q.FinalizeDeploymentRevisionByID(ctx, db.FinalizeDeploymentRevisionByIDParams{
		ID: pgUUID(revID), Status: status,
	}); err != nil {
		return false, fmt.Errorf("store: finalize deploy watch revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: finalize deploy watch commit: %w", err)
	}
	return true, nil
}

// CountActiveWatchesForCluster backs the cluster delete-guard (an in-flight watch
// also RESTRICTs the cluster FK).
func (s *Store) CountActiveWatchesForCluster(ctx context.Context, cluster string) (int64, error) {
	n, err := s.q.CountActiveWatchesForCluster(ctx, cluster)
	if err != nil {
		return 0, fmt.Errorf("store: count active watches for cluster %q: %w", cluster, err)
	}
	return n, nil
}
