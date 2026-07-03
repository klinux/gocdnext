package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

	var out []SupersededRun
	for _, c := range candidates { // already counter DESC from SQL
		superseded, err := s.supersedeOne(ctx, tx, q, c.id, c.counter, in, envsForGate)
		if err != nil {
			return nil, err
		}
		if superseded != nil {
			out = append(out, *superseded)
		}
	}
	return out, nil
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

	// Snapshot running jobs BEFORE flipping state so the caller can push
	// CancelJob frames (the agent's own JobResult clears agent_id).
	running, err := listRunningJobsForRunTx(ctx, tx, id)
	if err != nil {
		return nil, fmt.Errorf("store: supersede list running: %w", err)
	}
	if _, err := q.StampCancelRequestedAtForRun(ctx, pgUUID(id)); err != nil {
		return nil, fmt.Errorf("store: supersede stamp pending: %w", err)
	}
	reason := fmt.Sprintf("superseded by #%d", in.NewerCounter)
	if _, err := q.SupersedeRun(ctx, db.SupersedeRunParams{
		ID:           pgUUID(id),
		SupersededBy: pgUUID(in.NewerRunID),
		CancelReason: &reason,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // lost the flip race
		}
		return nil, fmt.Errorf("store: supersede run: %w", err)
	}
	if err := q.CancelQueuedStagesInRun(ctx, pgUUID(id)); err != nil {
		return nil, fmt.Errorf("store: supersede stages: %w", err)
	}
	if err := q.CancelQueuedJobsInRun(ctx, pgUUID(id)); err != nil {
		return nil, fmt.Errorf("store: supersede jobs: %w", err)
	}
	return &SupersededRun{RunID: id, Counter: counter, RunningJobs: running}, nil
}

// listRunningJobsForRunTx is the tx-scoped twin of listRunningJobsForRun.
func listRunningJobsForRunTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID) ([]RunningJobRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, agent_id
		FROM job_runs
		WHERE run_id = $1 AND status = 'running' AND agent_id IS NOT NULL
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunningJobRef
	for rows.Next() {
		var jobID, agentID uuid.UUID
		if err := rows.Scan(&jobID, &agentID); err != nil {
			return nil, err
		}
		out = append(out, RunningJobRef{JobID: jobID, AgentID: agentID})
	}
	return out, rows.Err()
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
