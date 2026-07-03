package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// SupersededRun is one run that a supersede pass canceled. The store does the DB
// terminalization in the caller's tx; the caller (a fire point at the api/grpc
// layer) fires the external effects AFTER commit — push CancelJob frames to
// RunningJobs, close the GitHub check, broadcast CleanupRunServices, run
// notifications. Mirrors CancelRun's "return the running jobs, let the handler
// signal them" split.
type SupersededRun struct {
	RunID       uuid.UUID
	Counter     int64
	RunningJobs []RunningJobRef
}

// supersedeInput carries what a fire point knows about the newer run N whose
// gate just became ready.
type supersedeInput struct {
	PipelineID   uuid.UUID
	Ref          string // lane ref (ignored when LaneMode == pipeline)
	LaneMode     string // domain.SupersedeBranch | domain.SupersedePipeline
	NewerRunID   uuid.UUID
	NewerCounter int64
	// ReadyEnvs is the concrete deploy env set N's just-ready gate governs
	// (domain.GovernedEnvs). Empty = the gate governs no deploy → whole-run
	// scope for the pile-clear (matches any candidate).
	ReadyEnvs []string
	// Def is the run's pipeline definition — the lane shares one pipeline, so
	// the same def resolves each victim's pending-gate env set.
	Def domain.Pipeline
}

// supersedeLaneSiblings cancels older pending runs in N's lane whose pending-gate
// env set intersects N's ready-gate env set. Runs INSIDE the caller's tx. Victims
// are locked + revalidated one at a time in counter-DESC order (so concurrent
// supersede passes acquire runs rows in one consistent descending order and can't
// deadlock), and terminalized via the same cascade CancelRun uses. Returns the
// superseded runs for the caller to fire external effects post-commit.
//
// Rollback note (#97): there is no runs.is_rollback — a rollback is a RerunJob on
// an EXISTING run (no new counter), so it never appears as a newer-run candidate.
// The "rollback survives a newer forward run" guarantee is the Phase-2 backstop's
// job.DeployRollback exemption, not a Phase-1 victim filter.
func (s *Store) supersedeLaneSiblings(ctx context.Context, tx pgx.Tx, in supersedeInput) ([]SupersededRun, error) {
	q := s.q.WithTx(tx)

	type cand struct {
		id      uuid.UUID
		counter int64
	}
	var candidates []cand
	if in.LaneMode == domain.SupersedePipeline {
		rows, err := q.SupersedeCandidatesPipeline(ctx, db.SupersedeCandidatesPipelineParams{
			PipelineID: pgUUID(in.PipelineID), Counter: in.NewerCounter,
		})
		if err != nil {
			return nil, fmt.Errorf("store: supersede candidates: %w", err)
		}
		for _, r := range rows {
			candidates = append(candidates, cand{fromPgUUID(r.ID), r.Counter})
		}
	} else {
		rows, err := q.SupersedeCandidatesBranch(ctx, db.SupersedeCandidatesBranchParams{
			PipelineID: pgUUID(in.PipelineID), Ref: in.Ref, Counter: in.NewerCounter,
		})
		if err != nil {
			return nil, fmt.Errorf("store: supersede candidates: %w", err)
		}
		for _, r := range rows {
			candidates = append(candidates, cand{fromPgUUID(r.ID), r.Counter})
		}
	}

	// Resolving a gate's governed-env set walks the definition DAG. A large stale
	// pile — the exact scenario this feature clears — would re-walk it per
	// candidate; memoize gate-name → envs across the whole pass (Def is constant).
	gateEnvMemo := make(map[string][]string)
	envsForGate := func(gate string) []string {
		if envs, ok := gateEnvMemo[gate]; ok {
			return envs
		}
		envs := in.Def.GovernedEnvs(gate)
		gateEnvMemo[gate] = envs
		return envs
	}

	// Cross-order deadlock guard (review MED). Supersede holds runs(V) FOR UPDATE
	// and then cancels the victim's gate/job rows — runs → job_runs order. But the
	// codebase's cascade (approval decideGate, result completion) takes them
	// job_runs → runs: it locks the gate/job row, then wants runs(V) via CompleteRun
	// (a rejected/failed cascade cancels the awaiting gate and finalizes the run).
	// The two orders can cycle. Rather than reorder the hot cascade path (and risk a
	// new approval↔result deadlock), BOUND every lock wait: a contended victim aborts
	// to its savepoint and is skipped, left pending for the next fire to retry (the
	// Phase-2 backstop is the hard guarantee; Phase-1 is a best-effort pile-clear).
	// This also kills the stale-cancel risk — an in-flight approval holds the gate
	// row, so supersede times out and bails instead of racing its decision. Scope
	// it to this pass with save+restore: a fire point that keeps writing on the SAME
	// tx after supersede must NOT inherit the short timeout (its writes would fail
	// with 55P03 unrelated to supersede). set_config(...,true) == SET LOCAL.
	var prevLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&prevLockTimeout); err != nil {
		return nil, fmt.Errorf("store: supersede read lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '75ms'`); err != nil {
		return nil, fmt.Errorf("store: supersede set lock_timeout: %w", err)
	}
	defer func() {
		// Best-effort: on an error return the caller rolls back the whole tx, so a
		// failed restore is harmless; on the success path this hands the outer tx
		// back its original timeout.
		_, _ = tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, prevLockTimeout)
	}()

	var out []SupersededRun
	for _, c := range candidates { // already counter DESC from SQL
		// One savepoint per victim so a lock-timeout/deadlock abort rolls back only
		// this victim's partial terminalization (incl. the runs-status flip) and the
		// loop continues with the outer tx healthy.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("store: supersede savepoint: %w", err)
		}
		superseded, err := s.supersedeOne(ctx, sp, s.q.WithTx(sp), c.id, c.counter, in, envsForGate)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isLockContention(err) {
				continue // contended (in-flight approval / failing cascade) — retry next fire
			}
			return nil, err
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, fmt.Errorf("store: supersede release savepoint: %w", err)
		}
		if superseded != nil {
			out = append(out, *superseded)
		}
	}
	return out, nil
}

// supersedeAfterCascade fires the #97 inline-gate supersede when a stage completion
// just made a downstream approval gate reachable. Called by CompleteJob
// (agent-driven) and decideGate (human-driven) AFTER cascadeAfterJobCompletion, in
// the SAME tx, so the supersede + its NOTIFY commit atomically with the completion.
// No-op unless a stage completed successfully, the pipeline opts into supersede, and
// the newly-frontier stage has a ready approval gate. Returns the victims (for the
// caller to surface/log); external effects fire via the run_superseded NOTIFY.
//
// The def load is gated behind comp.StageCompleted, so a job completion that doesn't
// finish a stage (the common case) pays nothing.
func (s *Store) supersedeAfterCascade(ctx context.Context, tx pgx.Tx, runID uuid.UUID, stageRunID uuid.UUID, comp *JobCompletion) ([]SupersededRun, error) {
	if !comp.StageCompleted || comp.StageStatus != string(domain.StatusSuccess) {
		return nil, nil
	}
	q := s.q.WithTx(tx)
	rc, err := q.GetRunSupersedeContext(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: supersede cascade context: %w", err)
	}
	var def domain.Pipeline
	if err := json.Unmarshal(rc.Definition, &def); err != nil {
		return nil, fmt.Errorf("store: supersede cascade decode: %w", err)
	}
	if def.Supersede != domain.SupersedeBranch && def.Supersede != domain.SupersedePipeline {
		return nil, nil
	}
	ordinal, err := q.GetStageRunOrdinal(ctx, pgUUID(stageRunID))
	if err != nil {
		return nil, fmt.Errorf("store: supersede cascade ordinal: %w", err)
	}
	readyEnvs, ready := def.ReadyGateEnvsAfterStage(int(ordinal))
	if !ready {
		return nil, nil
	}
	laneRef := ""
	if def.Supersede == domain.SupersedeBranch {
		laneRef = rc.Ref
	}
	return s.supersedeLaneSiblings(ctx, tx, supersedeInput{
		PipelineID:   fromPgUUID(rc.PipelineID),
		Ref:          laneRef,
		LaneMode:     def.Supersede,
		NewerRunID:   runID,
		NewerCounter: rc.Counter,
		ReadyEnvs:    readyEnvs,
		Def:          def,
	})
}

// SupersededAuditInfo returns the counters the run.superseded audit needs — the
// victim's own counter plus the superseding run's id + counter — for a run that
// was actually superseded. ok=false when the run has no superseded_by (a spurious
// or stale NOTIFY emits no audit). Used by the effects listener.
func (s *Store) SupersededAuditInfo(ctx context.Context, runID uuid.UUID) (SupersedeAuditInfo, bool, error) {
	row, err := s.q.SupersededAuditInfo(ctx, pgUUID(runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SupersedeAuditInfo{}, false, nil
	}
	if err != nil {
		return SupersedeAuditInfo{}, false, fmt.Errorf("store: superseded audit info: %w", err)
	}
	return SupersedeAuditInfo{
		SupersededCounter:  row.SupersededCounter,
		BySupersedingRunID: fromPgUUID(row.ByRunID),
		ByCounter:          row.ByCounter,
	}, true, nil
}

// SupersedeAuditInfo is the counter/id trio for the run.superseded audit — never a
// branch/ref value.
type SupersedeAuditInfo struct {
	SupersededCounter  int64
	BySupersedingRunID uuid.UUID
	ByCounter          int64
}

// ListRunningCancelRequestedForRun returns the still-running, cancel-requested
// jobs of a run (agent_id non-null) so the supersede effects listener can push a
// CancelJob frame to each. Reads committed state, so it's safe to call from the
// scheduler's NOTIFY handler after the supersede tx committed.
func (s *Store) ListRunningCancelRequestedForRun(ctx context.Context, runID uuid.UUID) ([]RunningJobRef, error) {
	rows, err := s.q.ListRunningCancelRequestedForRun(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list running cancel-requested: %w", err)
	}
	out := make([]RunningJobRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, RunningJobRef{JobID: fromPgUUID(r.ID), AgentID: fromPgUUID(r.AgentID)})
	}
	return out, nil
}

// isLockContention reports whether err is a bounded-wait abort we deliberately
// provoke to skip a contended victim: lock_timeout (55P03) from our SET LOCAL, or
// deadlock_detected (40P01) if Postgres's detector fires first. Either way the
// victim is left pending, not failed.
func isLockContention(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03" || pgErr.Code == "40P01"
	}
	return false
}

// supersedeOne locks + revalidates a single victim and terminalizes it if it
// still qualifies. Returns nil (no error) when the candidate raced out from under
// us (already terminal, gate decided, or env no longer intersects).
func (s *Store) supersedeOne(ctx context.Context, tx pgx.Tx, q *db.Queries, id uuid.UUID, counter int64, in supersedeInput, envsForGate func(string) []string) (*SupersededRun, error) {
	// Lock the victim row in the global order (runs → job_runs), then revalidate:
	// closes the race with a concurrent approve/cancel that could have moved it.
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // deleted under us
		}
		return nil, fmt.Errorf("store: supersede lock: %w", err)
	}
	if status != "queued" && status != "running" {
		return nil, nil // already terminal
	}

	gateNames, err := q.ListAwaitingGateNamesForRun(ctx, pgUUID(id))
	if err != nil {
		return nil, fmt.Errorf("store: supersede gate names: %w", err)
	}
	if len(gateNames) == 0 {
		return nil, nil // gate decided between candidate select and lock
	}
	// The victim's env set = every env its pending gates govern.
	seen := map[string]struct{}{}
	var victimEnvs []string
	for _, gn := range gateNames {
		for _, env := range envsForGate(gn) {
			if _, dup := seen[env]; !dup {
				seen[env] = struct{}{}
				victimEnvs = append(victimEnvs, env)
			}
		}
	}
	if !envSetsIntersect(in.ReadyEnvs, victimEnvs) {
		return nil, nil // different environment — not superseded (staging ≠ prod)
	}

	// Terminalize. THE ORDER IS LOAD-BEARING (review HIGH): flip the run + cancel
	// its queued stages/jobs BEFORE snapshotting running jobs. The scheduler's
	// AssignJob (scheduler.sql) is a bare `status='queued' AND agent_id IS NULL`
	// CAS that never checks runs.status and never locks the runs row — so our
	// `runs FOR UPDATE` does NOT serialize against it. CancelQueuedJobsInRun's
	// UPDATE contends on the same job_runs row instead: a job AssignJob won before
	// we locked it is committed-running and surfaces in the post-cancel stamp
	// below (→ CancelJob frame); a job we cancel first blocks a racing AssignJob,
	// which then re-evaluates its `status='queued'` predicate against the canceled
	// row and fails. Snapshotting FIRST reopens the gap where a job flips
	// queued→running between snapshot and cancel and keeps executing inside a
	// canceled run with no frame sent. (Deadlock-free: AssignJob is a single
	// autocommit statement that never also takes the runs lock — dispatch.go.)
	reason := fmt.Sprintf("superseded by #%d", in.NewerCounter)
	if _, err := q.SupersedeRun(ctx, db.SupersedeRunParams{
		ID:           pgUUID(id),
		SupersededBy: pgUUID(in.NewerRunID),
		CancelReason: &reason,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // can't happen under our FOR UPDATE; guarded anyway
		}
		return nil, fmt.Errorf("store: supersede run: %w", err)
	}
	if err := q.CancelQueuedStagesInRun(ctx, pgUUID(id)); err != nil {
		return nil, fmt.Errorf("store: supersede stages: %w", err)
	}
	if err := q.CancelQueuedJobsInRun(ctx, pgUUID(id)); err != nil {
		return nil, fmt.Errorf("store: supersede jobs: %w", err)
	}
	// Snapshot AFTER cancel: one UPDATE stamps cancel_requested_at on every
	// still-running job and RETURNS its (id, agent_id) as the CancelJob work-list.
	// By now every job of the run is either canceled (above) or committed-running
	// (AssignJob won the row before our cancel locked it), so this captures
	// exactly the jobs the caller must signal — no queued→running gap left.
	stamped, err := q.StampCancelRequestedAtForRun(ctx, pgUUID(id))
	if err != nil {
		return nil, fmt.Errorf("store: supersede stamp pending: %w", err)
	}
	running := make([]RunningJobRef, 0, len(stamped))
	for _, r := range stamped {
		running = append(running, RunningJobRef{JobID: fromPgUUID(r.ID), AgentID: fromPgUUID(r.AgentID)})
	}

	// Signal the effects listener (scheduler) to fire external effects for this
	// victim after commit — CancelJob frames + service cleanup. Emitted on the
	// per-victim savepoint tx, so a later lock-timeout bail that rolls the
	// savepoint back also discards the notification (Postgres drops NOTIFYs from
	// aborted subtransactions); it fires only if the whole tx commits.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, SupersededRunChannel, id.String()); err != nil {
		return nil, fmt.Errorf("store: supersede notify: %w", err)
	}
	return &SupersededRun{RunID: id, Counter: counter, RunningJobs: running}, nil
}

// envSetsIntersect reports whether two governed-env sets overlap. An empty set
// means "the gate governs no deploy" → whole-run scope for the Phase-1 pile-clear,
// which matches any candidate (a conservative over-cancel that never causes a
// stale deploy — that's Phase 2's job).
func envSetsIntersect(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(a))
	for _, e := range a {
		set[e] = struct{}{}
	}
	for _, e := range b {
		if _, ok := set[e]; ok {
			return true
		}
	}
	return false
}
