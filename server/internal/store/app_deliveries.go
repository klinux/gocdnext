package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrAppDeliveryAlreadyClaimed means the GitHub App webhook delivery id was
// already processed (a duplicate/redelivery, or a concurrent handler won the
// race). The rerun's transaction is rolled back — no second run is created.
var ErrAppDeliveryAlreadyClaimed = errors.New("store: github app delivery already claimed")

// RerunForAppDelivery re-runs runID and records the GitHub App webhook delivery
// as processed — ATOMICALLY. The delivery claim, the run creation, and the
// run_id link all commit in ONE transaction (via a run-tx hook), so:
//   - a crash mid-flight rolls back everything (no orphan run, no stuck claim);
//   - a duplicate/concurrent delivery loses the PK-insert race and its whole
//     transaction rolls back → ErrAppDeliveryAlreadyClaimed, no second run.
//
// The terminal guard (ErrRunActive) and not-rerunnable errors surface from the
// shared rerun path before the claim, so those never leave a ledger row.
func (s *Store) RerunForAppDelivery(ctx context.Context, runID uuid.UUID, deliveryID, event, triggeredBy string) (RunCreated, error) {
	hook := func(ctx context.Context, q *db.Queries, newRunID uuid.UUID) error {
		n, err := q.ClaimGithubAppDelivery(ctx, db.ClaimGithubAppDeliveryParams{
			DeliveryID: deliveryID,
			Event:      event,
		})
		if err != nil {
			return fmt.Errorf("store: claim app delivery: %w", err)
		}
		if n == 0 {
			// Already claimed (duplicate/concurrent) — abort so the whole run
			// creation rolls back.
			return ErrAppDeliveryAlreadyClaimed
		}
		if err := q.SetGithubAppDeliveryRun(ctx, db.SetGithubAppDeliveryRunParams{
			DeliveryID: deliveryID,
			RunID:      pgUUID(newRunID),
		}); err != nil {
			return fmt.Errorf("store: link app delivery run: %w", err)
		}
		return nil
	}
	return s.rerunRun(ctx, RerunRunInput{RunID: runID, TriggeredBy: triggeredBy}, hook)
}

// SweepAppDeliveries prunes ledger rows older than the cutoff and returns the
// count deleted. Retention housekeeping — safe on any cadence.
func (s *Store) SweepAppDeliveries(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-olderThan), Valid: true}
	n, err := s.q.DeleteOldGithubAppDeliveries(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: sweep app deliveries: %w", err)
	}
	return n, nil
}
