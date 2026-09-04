package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// The following three are ALL benign, expected outcomes of the freeze pause
// (#208) — like ErrApprovalGateDecided, they mean "touch nothing, retry on a
// later sweep", never a failure. The expirer maps each to a no-op.
//
//   - ErrApprovalGateFrozen: a governed environment is currently frozen, so the
//     gate must NOT expire — the freeze exists precisely to hold it.
//   - ErrApprovalGateContended: a concurrent freeze/unfreeze (or deploy
//     admission) holds the per-(project, env) advisory lock; the expiry's
//     non-blocking try-lock backed off rather than wait.
//   - ErrApprovalGateWithinWindow: a recent unfreeze pushed effective_start
//     forward (the last_unfrozen_at floor), so the gate is inside a fresh window.
var (
	ErrApprovalGateFrozen       = errors.New("store: approval gate governed env frozen")
	ErrApprovalGateContended    = errors.New("store: approval gate freeze lock contended")
	ErrApprovalGateWithinWindow = errors.New("store: approval gate within fresh window")
)

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
// Also returns the gate's GovernedFreezeEnvs — the environments a freeze on
// which must PAUSE this gate's expiry (#208). Computed from the SAME immutable
// definition snapshot as the window (no extra query, no race: which envs a gate
// governs is fixed by the snapshot; only WHETHER they are frozen is checked
// authoritatively in-tx by ExpireApprovalGate). Empty when the gate governs no
// deploy/migration env.
func (s *Store) ResolveApprovalWindow(ctx context.Context, runID uuid.UUID, jobName string, serverDefault time.Duration) (time.Duration, []string, bool, error) {
	rc, err := s.q.GetRunSupersedeContext(ctx, pgUUID(runID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, false, nil // run vanished under us — nothing to expire
		}
		return 0, nil, false, fmt.Errorf("store: approval window context: %w", err)
	}
	if len(rc.Definition) == 0 {
		return 0, nil, false, nil
	}
	var def domain.Pipeline
	if err := json.Unmarshal(rc.Definition, &def); err != nil {
		return 0, nil, false, fmt.Errorf("store: approval window decode: %w", err)
	}
	for i := range def.Jobs {
		if def.Jobs[i].Name != jobName || def.Jobs[i].Approval == nil {
			continue
		}
		d, ok := def.Jobs[i].Approval.EffectiveApprovalTimeout(serverDefault)
		return d, def.GovernedFreezeEnvs(jobName), ok, nil
	}
	return 0, nil, false, nil
}

// ExpireApprovalGateInput carries everything the expiry tx needs to make the
// freeze decision authoritatively in-transaction. FreezeEnvs / Window /
// AwaitingSince are resolved by the caller from the run's immutable definition
// snapshot (ResolveApprovalWindow); PipelineID resolves project_id in-tx.
type ExpireApprovalGateInput struct {
	JobRunID      uuid.UUID
	RunID         uuid.UUID
	PipelineID    uuid.UUID
	FreezeEnvs    []string      // the gate's GovernedFreezeEnvs (may be empty)
	Window        time.Duration // the resolved expiry window (never/0 gates don't reach here)
	AwaitingSince time.Time     // the value the candidate scan observed (TOCTOU + floor base)
	Reason        string
}

// ExpireApprovalGate cancels the run behind an abandoned gate, in ONE
// transaction, and returns the still-running jobs the caller must signal.
//
// The whole cascade mirrors supersedeOne's terminalizer rather than reusing
// CancelRun, for two reasons: the gate stamp and the run flip must be atomic
// with each other (a crash between them would leave a canceled run with no
// explanation of why), and the run needs a cancel_reason CancelRun doesn't set.
//
// #208: an environment freeze must PAUSE this — cancelling a gate a freeze is
// deliberately holding is the exact bug. The check is AUTHORITATIVE in-tx (a
// caller-side scan races a concurrent freeze) and follows the global lock order
// lane -> freeze -> row: (1) resolve project_id, (2) take the per-(project, env)
// freeze advisory locks (non-blocking), (3) lock the run, (4) reject if any
// governed env is frozen, (5) reject if a recent unfreeze granted a fresh window.
//
// Returns ErrApprovalGateDecided (human decided), ErrRunAlreadyTerminal (run
// moved terminal), and the three benign freeze outcomes (Frozen / Contended /
// WithinWindow). All mean "do nothing", all are expected under normal operation.
func (s *Store) ExpireApprovalGate(ctx context.Context, in ExpireApprovalGateInput) (ExpireApprovalGateResult, error) {
	jobRunID, runID, reason := in.JobRunID, in.RunID, in.Reason

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// (1) PIPELINE — resolve project_id authoritatively in-tx: the candidate
	// carries only pipeline_id, and the freeze lock key + probe need the project.
	// pipeline -> project is immutable, so no lock is required for this read.
	pgProject, err := q.GetPipelineProjectID(ctx, pgUUID(in.PipelineID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExpireApprovalGateResult{}, ErrRunNotFound // pipeline gone — nothing to expire
		}
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: project: %w", err)
	}
	projectID := fromPgUUID(pgProject)

	// (2) FREEZE — take the per-(project, env) freeze advisory locks, in name
	// order, NON-BLOCKING. This is step (2) of the global lock order (lane ->
	// freeze -> row); the expirer takes no lane locks. try-lock (not the blocking
	// pg_advisory_xact_lock the freeze/unfreeze/deploy paths use) so the expiry
	// can never be the victim in a deadlock cycle — contention just backs off and
	// retries next sweep. Sorted so the acquisition order is deterministic; xact
	// advisory locks are reentrant, so any duplicate env is a harmless no-op.
	envs := slices.Clone(in.FreezeEnvs)
	slices.Sort(envs)
	for _, env := range envs {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`,
			ProjectEnvFreezeLockKey(projectID, env)).Scan(&got); err != nil {
			return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: freeze lock: %w", err)
		}
		if !got {
			return ExpireApprovalGateResult{}, ErrApprovalGateContended
		}
	}

	// (3) RUN — lock the run in the global runs -> job_runs order (the same order
	// supersede takes) and revalidate. Bounding the wait is deliberate: the
	// approval decide path and the result cascade take these rows in the OPPOSITE
	// order, so an unbounded wait here could cycle. An expiry that loses the race
	// is simply retried on the next sweep — a 7-day window has no urgency, and
	// skipping is always safe.
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

	// (4) FROZEN? — under the held locks, any governed env currently frozen means
	// the gate must NOT expire: the freeze exists to hold it. Abort, touch nothing.
	if frozen, err := frozenGovernedEnvsTx(ctx, q, projectID, envs); err != nil {
		return ExpireApprovalGateResult{}, err
	} else if len(frozen) > 0 {
		return ExpireApprovalGateResult{}, ErrApprovalGateFrozen
	}

	// (5) FLOOR — a recent unfreeze grants a fresh window. effective_start =
	// max(awaiting_since, MAX(last_unfrozen_at) over the governed envs); reject if
	// clock_timestamp() (measured AFTER the locks, real wall time) is still inside
	// effective_start + window. Read under the freeze locks so it is serialized
	// against a concurrent unfreeze upsert. No envs / no floor row => no MAX =>
	// falls back to awaiting_since, i.e. the pre-#208 behaviour.
	if len(envs) > 0 {
		floor, err := q.MaxLastUnfrozenAt(ctx, db.MaxLastUnfrozenAtParams{
			ProjectID: pgProject, Environments: envs,
		})
		if err != nil {
			return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: floor: %w", err)
		}
		effectiveStart := in.AwaitingSince
		if floor.Valid && floor.Time.After(effectiveStart) {
			effectiveStart = floor.Time
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: clock: %w", err)
		}
		if now.Before(effectiveStart.Add(in.Window)) {
			return ExpireApprovalGateResult{}, ErrApprovalGateWithinWindow
		}
	}

	// Stamp the gate FIRST. Its guard (status='awaiting_approval' AND the exact
	// (run_id, awaiting_since) the candidate scan saw) is the TOCTOU check against
	// a human deciding OR a rerun re-parking the gate between the scan and now: no
	// rows means the state moved under us and the whole expiry aborts, rolling
	// back rather than cancelling a run somebody just approved or re-armed.
	if _, err := q.MarkApprovalGateExpired(ctx, db.MarkApprovalGateExpiredParams{
		ID:            pgUUID(jobRunID),
		RunID:         pgUUID(runID),
		AwaitingSince: pgtype.Timestamptz{Time: in.AwaitingSince, Valid: true},
	}); err != nil {
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
	// together and no "ready to approve" ghost survives in the UI. Origin
	// approval_expiry (#208) so an upstream rerun revives these (a system
	// cancel, not a deliberate user_job single-job cancel).
	if err := q.CancelQueuedJobsInRun(ctx, db.CancelQueuedJobsInRunParams{
		RunID:  pgUUID(runID),
		Origin: nullableString(string(cancelOriginApprovalExpiry)),
	}); err != nil {
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
	stamped, err := q.StampCancelRequestedAtForRun(ctx, db.StampCancelRequestedAtForRunParams{
		RunID:  pgUUID(runID),
		Origin: nullableString(string(cancelOriginApprovalExpiry)),
	})
	if err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: stamp running: %w", err)
	}
	// Only AGENT-owned jobs go into the CancelJob fanout. Since #207 dropped the
	// agent_id filter from StampCancelRequestedAtForRun, the RETURNING set now also
	// includes server-managed native jobs (agent_id NULL) — their cancel_requested_at
	// stamp is durable and the watcher/reaper finalizes them, but framing one would
	// mean Dispatch(uuid.Nil) → a spurious ErrNoSession + misleading WARN every time
	// an expiry catches a native deploy mid-flight. Same split as CancelJobRun.
	running := make([]RunningJobRef, 0, len(stamped))
	for _, r := range stamped {
		if !r.AgentID.Valid {
			continue
		}
		running = append(running, RunningJobRef{JobID: fromPgUUID(r.ID), AgentID: fromPgUUID(r.AgentID)})
	}
	if err := q.NotifyRunTerminalEffects(ctx, db.NotifyRunTerminalEffectsParams{
		Channel: RunTerminalEffectsChannel,
		Payload: runID.String(),
	}); err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: notify terminal effects: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ExpireApprovalGateResult{}, fmt.Errorf("store: expire approval gate: commit: %w", err)
	}
	return ExpireApprovalGateResult{
		RunningJobs:       running,
		ServiceGeneration: row.ServiceGeneration,
	}, nil
}
