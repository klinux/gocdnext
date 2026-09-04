package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// RunTerminalEffectsChannel is emitted transactionally when a run reaches a
// terminal status through the generic cascade/cancel paths. The scheduler owns
// the durable claim/replay worker behind it.
const RunTerminalEffectsChannel = "run_terminal_effects"

// RunTerminalEffectsClaim is the worker's lease payload. ServiceGeneration is
// the generation observed while claiming; MarkRunTerminalEffectsDone requires
// the same generation so a stale worker cannot complete a post-rerun terminal
// event for the same run_id.
type RunTerminalEffectsClaim struct {
	Status            string
	ServiceGeneration int64
	FirstClaim        bool
}

func (s *Store) ClaimRunTerminalEffects(ctx context.Context, runID uuid.UUID) (RunTerminalEffectsClaim, bool, error) {
	row, err := s.q.ClaimRunTerminalEffects(ctx, db.ClaimRunTerminalEffectsParams{
		ID:    pgUUID(runID),
		Lease: pgtype.Interval{Microseconds: supersedeEffectsLease.Microseconds(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RunTerminalEffectsClaim{}, false, nil
	}
	if err != nil {
		return RunTerminalEffectsClaim{}, false, fmt.Errorf("store: claim run terminal effects: %w", err)
	}
	return RunTerminalEffectsClaim{
		Status:            row.Status,
		ServiceGeneration: row.ServiceGeneration,
		FirstClaim:        row.FirstClaim,
	}, true, nil
}

func (s *Store) MarkRunTerminalEffectsDone(ctx context.Context, runID uuid.UUID, serviceGeneration int64) (bool, error) {
	n, err := s.q.MarkRunTerminalEffectsDone(ctx, db.MarkRunTerminalEffectsDoneParams{
		ID:                pgUUID(runID),
		ServiceGeneration: serviceGeneration,
	})
	if err != nil {
		return false, fmt.Errorf("store: mark run terminal effects done: %w", err)
	}
	return n > 0, nil
}

func (s *Store) ListPendingRunTerminalEffects(ctx context.Context, maxRows int32) ([]uuid.UUID, error) {
	if maxRows <= 0 {
		maxRows = 100
	}
	rows, err := s.q.ListPendingRunTerminalEffects(ctx, db.ListPendingRunTerminalEffectsParams{
		Lease:   pgtype.Interval{Microseconds: supersedeEffectsLease.Microseconds(), Valid: true},
		MaxRows: maxRows,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pending run terminal effects: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, id := range rows {
		out = append(out, fromPgUUID(id))
	}
	return out, nil
}

func (s *Store) NotifyRunTerminalEffects(ctx context.Context, runID uuid.UUID) error {
	if err := s.q.NotifyRunTerminalEffects(ctx, db.NotifyRunTerminalEffectsParams{
		Channel: RunTerminalEffectsChannel,
		Payload: runID.String(),
	}); err != nil {
		return fmt.Errorf("store: notify run terminal effects: %w", err)
	}
	return nil
}
