package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// DisruptedOutcome is the verdict for a job the agent reported DISRUPTED
// (task pod preempted / evicted / node-reclaimed). It drives the handler's
// metric label and whether the run needs a re-dispatch NOTIFY.
type DisruptedOutcome string

const (
	// DisruptedRequeued: a plain job under the cap → re-dispatched (attempt+1).
	DisruptedRequeued DisruptedOutcome = "requeued"
	// DisruptedFailedCapped: the retry cap was already reached → terminal fail.
	DisruptedFailedCapped DisruptedOutcome = "failed_capped"
	// DisruptedFailedUnsafe: a deploy/environment job (retry_unsafe) → NOT
	// auto-retried, terminal fail; a human reruns if it's safe to.
	DisruptedFailedUnsafe DisruptedOutcome = "failed_unsafe_target"
	// DisruptedCanceled: an operator cancel won the race (cancel_requested_at
	// stamped) → terminal 'canceled' (never counts as a failure).
	DisruptedCanceled DisruptedOutcome = "canceled"
	// DisruptedSkipped: the row is already terminal, or its (agent, attempt)
	// snapshot moved (a concurrent reaper / rerun owns it) → no action taken.
	DisruptedSkipped DisruptedOutcome = "skipped"
)

// HandleDisruptedInput carries the (agent, attempt) snapshot the gRPC
// result handler observed for the disrupted job.
type HandleDisruptedInput struct {
	JobRunID        uuid.UUID
	ExpectedAgentID uuid.UUID
	ExpectedAttempt int32
	MaxAttempts     int32
	// Reason is the agent's disruption message, stored as the job error on a
	// terminal fail so an operator sees "…node preemption/eviction…".
	Reason string
}

// DisruptedResult reports the verdict and what the caller must do next.
type DisruptedResult struct {
	Outcome DisruptedOutcome
	// RunID is set on every non-error result — the caller uses it to emit
	// NotifyRunQueued when Outcome == DisruptedRequeued.
	RunID uuid.UUID
	// Completion is set on terminal outcomes (failed_*/canceled); the
	// stage/run cascade already ran inside CompleteJob.
	Completion *JobCompletion
}

// HandleDisruptedJob decides what to do with a job the agent reported
// DISRUPTED, reusing the reaper's requeue primitive (requeueStaleJob) and
// the normal terminal-completion primitive (CompleteJob):
//
//   - retry_unsafe (deploy/environment)  → terminal fail (evidence PRESERVED)
//   - operator cancel won the race        → terminal 'canceled'
//   - retry cap reached                   → terminal fail (evidence preserved)
//   - otherwise (plain job, under cap)    → requeue (attempt+1), NO notify
//
// It deliberately never NOTIFYs: the caller frees the session's assignment
// + capacity FIRST, THEN emits NotifyRunQueued — otherwise the scheduler's
// LISTEN wake could redispatch the requeued job onto the just-freed slot
// while the old (jobID → attempt) assignment is still recorded, failing the
// RecordAssignmentCAS.
//
// The classifying read is unlocked; correctness rests on the ACTION's
// snapshot CAS — requeueStaleJob (ReclaimJobForRetry) and CompleteJob
// (CompleteJobRun) both validate (agent_id, attempt). A row that moved out
// from under us fails that CAS and collapses to DisruptedSkipped.
//
// Terminal uses CompleteJob, NOT FailStaleJobAtMax: CompleteJob preserves
// evidence AND resolves the EFFECTIVE status (a cancel_requested_at row
// lands 'canceled'), whereas FailStaleJobAtMax refuses cancel rows — which
// would leave a cancel+disruption job hanging.
func (s *Store) HandleDisruptedJob(ctx context.Context, in HandleDisruptedInput) (DisruptedResult, error) {
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	cls, err := s.q.GetJobRunForDisruption(ctx, pgUUID(in.JobRunID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DisruptedResult{Outcome: DisruptedSkipped}, nil
		}
		return DisruptedResult{}, fmt.Errorf("store: disrupted classify: %w", err)
	}
	runID := fromPgUUID(cls.RunID)

	// Already terminal (a concurrent reaper / earlier result won) → nothing
	// to do; the caller won't touch the session.
	if cls.Status != string(domain.StatusRunning) {
		return DisruptedResult{Outcome: DisruptedSkipped, RunID: runID}, nil
	}

	canceled := cls.CancelRequestedAt.Valid
	// Requeue only a plain job, no cancel intent, still under the cap.
	if !cls.RetryUnsafe && !canceled && cls.Attempt+1 <= maxAttempts {
		var res ReclaimResult
		if err := s.requeueStaleJob(ctx, in.JobRunID, maxAttempts,
			in.ExpectedAttempt, in.ExpectedAgentID, false /*notify*/, &res); err != nil {
			return DisruptedResult{}, fmt.Errorf("store: disrupted requeue: %w", err)
		}
		if res.Action == ReclaimActionRequeued {
			return DisruptedResult{Outcome: DisruptedRequeued, RunID: runID}, nil
		}
		// ReclaimActionSkipped: raced (cancel just landed / reclaimed /
		// snapshot moved). Fall through to the terminal path below, which
		// resolves the effective status.
	}

	// Terminal. Evidence preserved; effective status resolved (cancel → 'canceled').
	comp, ok, err := s.CompleteJob(ctx, CompleteJobInput{
		JobRunID:        in.JobRunID,
		Status:          string(domain.StatusFailed),
		ExitCode:        -1,
		ErrorMsg:        in.Reason,
		ExpectedAgentID: in.ExpectedAgentID,
		ExpectedAttempt: in.ExpectedAttempt,
	})
	if err != nil {
		return DisruptedResult{}, fmt.Errorf("store: disrupted terminal: %w", err)
	}
	if !ok {
		return DisruptedResult{Outcome: DisruptedSkipped, RunID: runID}, nil
	}

	outcome := DisruptedFailedCapped
	switch {
	case comp.JobStatus == string(domain.StatusCanceled):
		// A cancel won the DB race — record it as a cancel, not a failure.
		outcome = DisruptedCanceled
	case cls.RetryUnsafe:
		outcome = DisruptedFailedUnsafe
	}
	return DisruptedResult{Outcome: outcome, RunID: runID, Completion: &comp}, nil
}
