package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// AppDeliveryClaim is the outcome of trying to claim a GitHub App webhook
// delivery for processing — the exactly-once gate for App re-runs.
type AppDeliveryClaim int

const (
	// AppDeliveryClaimed — this caller now owns the delivery; proceed, then
	// finish with MarkAppDeliveryDone (success) or ReleaseAppDelivery (failure).
	AppDeliveryClaimed AppDeliveryClaim = iota
	// AppDeliveryDone — already processed to completion; skip.
	AppDeliveryDone
	// AppDeliveryInFlight — another handler holds a fresh claim; skip.
	AppDeliveryInFlight
)

// ClaimAppDelivery is the idempotency gate for GitHub App webhook deliveries
// (keyed by X-GitHub-Delivery). Exactly one caller gets AppDeliveryClaimed for a
// given id: the PK INSERT is the mutual-exclusion point, so two concurrent
// redeliveries can't both claim. A prior attempt that crashed leaves a stale
// 'processing' row; a redelivery older than staleAfter re-claims it (also atomic
// — the conditional UPDATE's RowsAffected settles the race).
func (s *Store) ClaimAppDelivery(ctx context.Context, deliveryID, event string, staleAfter time.Duration) (AppDeliveryClaim, error) {
	n, err := s.q.ClaimGithubAppDelivery(ctx, db.ClaimGithubAppDeliveryParams{DeliveryID: deliveryID, Event: event})
	if err != nil {
		return 0, fmt.Errorf("store: claim app delivery: %w", err)
	}
	if n == 1 {
		return AppDeliveryClaimed, nil
	}
	// A row already exists — inspect it.
	row, err := s.q.GetGithubAppDelivery(ctx, deliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Raced against a release/retention delete between our INSERT and read;
		// a later redelivery will settle it.
		return AppDeliveryInFlight, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: get app delivery: %w", err)
	}
	if row.Status == "done" {
		return AppDeliveryDone, nil
	}
	// status == 'processing': re-claim only if stale (the prior attempt crashed).
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-staleAfter), Valid: true}
	won, err := s.q.ReclaimStaleGithubAppDelivery(ctx, db.ReclaimStaleGithubAppDeliveryParams{
		DeliveryID: deliveryID,
		UpdatedAt:  cutoff,
	})
	if err != nil {
		return 0, fmt.Errorf("store: reclaim stale app delivery: %w", err)
	}
	if won == 1 {
		return AppDeliveryClaimed, nil
	}
	return AppDeliveryInFlight, nil
}

// MarkAppDeliveryDone records that a claimed delivery finished, linking the run
// it produced. Redeliveries then short-circuit as AppDeliveryDone.
func (s *Store) MarkAppDeliveryDone(ctx context.Context, deliveryID string, runID uuid.UUID) error {
	if err := s.q.MarkGithubAppDeliveryDone(ctx, db.MarkGithubAppDeliveryDoneParams{
		DeliveryID: deliveryID,
		RunID:      pgUUID(runID),
	}); err != nil {
		return fmt.Errorf("store: mark app delivery done: %w", err)
	}
	return nil
}

// ReleaseAppDelivery drops a claim after the work failed, so GitHub's automatic
// redelivery of the SAME id can retry cleanly.
func (s *Store) ReleaseAppDelivery(ctx context.Context, deliveryID string) error {
	if err := s.q.ReleaseGithubAppDelivery(ctx, deliveryID); err != nil {
		return fmt.Errorf("store: release app delivery: %w", err)
	}
	return nil
}

// SweepAppDeliveries prunes completed ledger rows older than the cutoff and
// returns the count deleted. Retention housekeeping — safe on any cadence.
func (s *Store) SweepAppDeliveries(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-olderThan), Valid: true}
	n, err := s.q.DeleteOldGithubAppDeliveries(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: sweep app deliveries: %w", err)
	}
	return n, nil
}
