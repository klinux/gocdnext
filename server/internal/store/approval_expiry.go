package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// PendingApprovalGate is one candidate row for the approval expirer: a gate
// still parked in `awaiting_approval` long enough that its shortest possible
// window has elapsed. Whether it ACTUALLY expired depends on the window in the
// run's pipeline definition, which the caller resolves — see
// ResolveApprovalWindow.
type PendingApprovalGate struct {
	JobRunID      uuid.UUID
	RunID         uuid.UUID
	JobName       string
	AwaitingSince time.Time
	PipelineID    uuid.UUID
	Counter       int64
}

// ExpireApprovalGateResult reports what the expiry touched, mirroring
// CancelRunResult — the caller pushes CancelJob frames to RunningJobs and
// carries ServiceGeneration as the cleanup's max_generation.
type ExpireApprovalGateResult struct {
	RunningJobs       []RunningJobRef
	ServiceGeneration int64
}

// ErrApprovalGateDecided means the gate left `awaiting_approval` between the
// candidate scan and the expiry write — a human approved, rejected, or
// canceled it under us. Not an error condition: the decision the expirer
// existed to force already happened, so it backs off and touches nothing.
var ErrApprovalGateDecided = errors.New("store: approval gate already decided")

// ApprovalGateCursor is the keyset position of a candidate scan. The zero
// value starts at the beginning of the queue.
//
// The job-run id is part of the position because awaiting_since is NOT unique:
// every gate of a run is stamped in a single transaction, so siblings share a
// timestamp. A timestamp-only cursor would either re-serve them forever or skip
// past a whole group.
type ApprovalGateCursor struct {
	Since time.Time
	ID    uuid.UUID
}

// Next returns the cursor positioned just after the given candidate.
func (c ApprovalGateCursor) Next(g PendingApprovalGate) ApprovalGateCursor {
	return ApprovalGateCursor{Since: g.AwaitingSince, ID: g.JobRunID}
}

// ListPendingApprovalGates returns one keyset page of gates awaiting a decision
// since before `olderThan`, ordered (awaiting_since, id) ascending and starting
// strictly after `cursor`.
//
// `olderThan` must be the SHORTEST window that could apply to any gate
// (domain.ApprovalTimeoutMin), not the server default — a gate may declare a
// much shorter window of its own and would otherwise never surface.
//
// Paging (rather than one capped query) is a correctness requirement: whether a
// candidate actually expired is only knowable after reading the run definition
// in Go, so a single `LIMIT n` would truncate before that decision and let n
// never-expiring gates permanently hide an expirable one behind them.
func (s *Store) ListPendingApprovalGates(ctx context.Context, olderThan time.Time, cursor ApprovalGateCursor, pageSize int32) ([]PendingApprovalGate, error) {
	rows, err := s.q.ListPendingApprovalGates(ctx, db.ListPendingApprovalGatesParams{
		OlderThan:   pgtype.Timestamptz{Time: olderThan, Valid: true},
		CursorSince: pgtype.Timestamptz{Time: cursor.Since, Valid: true},
		CursorID:    pgUUID(cursor.ID),
		PageSize:    pageSize,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pending approval gates: %w", err)
	}
	out := make([]PendingApprovalGate, 0, len(rows))
	for _, r := range rows {
		var since time.Time
		if t := pgTimePtr(r.AwaitingSince); t != nil {
			since = *t
		}
		out = append(out, PendingApprovalGate{
			JobRunID:      fromPgUUID(r.ID),
			RunID:         fromPgUUID(r.RunID),
			JobName:       r.Name,
			AwaitingSince: since,
			PipelineID:    fromPgUUID(r.PipelineID),
			Counter:       r.Counter,
		})
	}
	return out, nil
}

// ResolveApprovalWindow reads the run's OWN definition snapshot and returns the
// effective expiry window for one gate, or ok=false when the gate must never
// expire (explicit `timeout: never`, no server default, or the job is no longer
// a gate in the definition).
//
// runs.definition — NOT pipelines.definition — on purpose: the same reason the
// supersede gate-graph reads it. A later ApplyProject that edits the live
// pipeline must not retroactively change the window a already-parked gate was
// materialised under.
//
// A definition that no longer contains the job (renamed between apply and now)
// resolves to "never expire": refusing to cancel a run we can't fully explain
// is the fail-safe direction for a destructive action.
func (s *Store) ResolveApprovalWindow(ctx context.Context, runID uuid.UUID, jobName string, serverDefault time.Duration) (time.Duration, bool, error) {
	rc, err := s.q.GetRunSupersedeContext(ctx, pgUUID(runID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil // run vanished under us — nothing to expire
		}
		return 0, false, fmt.Errorf("store: approval window context: %w", err)
	}
	if len(rc.Definition) == 0 {
		return 0, false, nil
	}
	var def domain.Pipeline
	if err := json.Unmarshal(rc.Definition, &def); err != nil {
		return 0, false, fmt.Errorf("store: approval window decode: %w", err)
	}
	for i := range def.Jobs {
		if def.Jobs[i].Name != jobName || def.Jobs[i].Approval == nil {
			continue
		}
		d, ok := def.Jobs[i].Approval.EffectiveApprovalTimeout(serverDefault)
		return d, ok, nil
	}
	return 0, false, nil
}

// ExpireApprovalGate cancels the run behind an abandoned gate, in ONE
// transaction, and returns the still-running jobs the caller must signal.
//
// The whole cascade mirrors supersedeOne's terminalizer rather than reusing
// CancelRun, for two reasons: the gate stamp and the run flip must be atomic
// with each other (a crash between them would leave a canceled run with no
// explanation of why), and the run needs a cancel_reason CancelRun doesn't set.
//
// Returns ErrApprovalGateDecided when a human decided the gate under us, and
// ErrRunAlreadyTerminal when the run moved terminal by another path. Both mean
// "do nothing", and both are expected under normal concurrency.
func (s *Store) ExpireApprovalGate(ctx context.Context, jobRunID, runID uuid.UUID, reason string) (ExpireApprovalGateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Lock the run in the global runs -> job_runs order (the same order
	// supersede takes) and revalidate. Bounding the wait is deliberate: the
	// approval decide path and the result cascade take these rows in the
	// OPPOSITE order, so an unbounded wait here could cycle. An expiry that
	// loses the race is simply retried on the next sweep — a 7-day window has
	// no urgency, and skipping is always safe.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '75ms'`); err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: lock timeout: %w", err)
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, pgUUID(runID)).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExpireApprovalGateResult{}, ErrRunNotFound
		}
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: lock run: %w", err)
	}
	if status != "queued" && status != "running" {
		return ExpireApprovalGateResult{}, ErrRunAlreadyTerminal
	}

	// Stamp the gate FIRST. Its `status = 'awaiting_approval'` guard is the
	// race check against a human deciding between the candidate scan and now:
	// no rows means the decision already happened and the whole expiry aborts,
	// rolling back rather than cancelling a run somebody just approved.
	if _, err := q.MarkApprovalGateExpired(ctx, pgUUID(jobRunID)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExpireApprovalGateResult{}, ErrApprovalGateDecided
		}
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: stamp: %w", err)
	}

	row, err := q.ExpireApprovalRun(ctx, db.ExpireApprovalRunParams{
		ID:           pgUUID(runID),
		CancelReason: &reason,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExpireApprovalGateResult{}, ErrRunAlreadyTerminal
		}
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: cancel run: %w", err)
	}

	if err := q.CancelQueuedStagesInRun(ctx, pgUUID(runID)); err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: stages: %w", err)
	}
	// Also flips the gate row itself to canceled (its predicate covers
	// awaiting_approval), so the decision stamp above and the status land
	// together and no "ready to approve" ghost survives in the UI.
	if err := q.CancelQueuedJobsInRun(ctx, pgUUID(runID)); err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: jobs: %w", err)
	}

	// Snapshot AFTER the queued cancel, for the same load-bearing reason
	// CancelRun and supersedeOne document: AssignJob is a bare
	// `status='queued'` CAS that never consults runs.status, so a job can flip
	// queued->running concurrently. Cancelling first forces the contention onto
	// the shared job_runs row; by now every job is either canceled or
	// committed-running, and this stamp returns exactly the CancelJob work-list.
	// Gates are not the only jobs in a run — a stage-0 build can still be
	// executing while a later gate times out.
	stamped, err := q.StampCancelRequestedAtForRun(ctx, pgUUID(runID))
	if err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: stamp running: %w", err)
	}
	running := make([]RunningJobRef, 0, len(stamped))
	for _, r := range stamped {
		running = append(running, RunningJobRef{JobID: fromPgUUID(r.ID), AgentID: fromPgUUID(r.AgentID)})
	}

	if err := tx.Commit(ctx); err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: commit: %w", err)
	}
	return ExpireApprovalGateResult{
		RunningJobs:       running,
		ServiceGeneration: row.ServiceGeneration,
	}, nil
}
