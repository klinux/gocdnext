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

// ReclaimAction names the reaper's verdict for a stale job.
type ReclaimAction string

const (
	ReclaimActionRequeued ReclaimAction = "requeued"
	ReclaimActionFailed   ReclaimAction = "failed"
	ReclaimActionSkipped  ReclaimAction = "skipped"
)

// ReclaimResult is the per-job summary returned by ReclaimStaleJobs
// and ReclaimAgentJobs.
//
// AgentSessionGeneration is the per-agent monotonic counter the
// reaper snapshotted from `agents.session_generation` at SELECT
// time. The reaper-side fencer uses it as the CAS predicate when
// closing the stale in-memory session: a successor Register that
// bumped the counter between the SELECT and the fence call means
// the session we'd be revoking is the NEW healthy one — the
// generation mismatch makes the fence a no-op in that case.
//
// Zero when the row has no agent (defensive category 2) or when
// the field wasn't populated (register-fence path; agent_service
// drives session closure directly there, no generation needed).
type ReclaimResult struct {
	JobRunID               uuid.UUID
	RunID                  uuid.UUID
	JobName                string
	AgentID                uuid.UUID
	AgentSessionGeneration int64
	Attempt                int32
	Action                 ReclaimAction
	Err                    error
}

// MarkAgentSeen bumps agents.last_seen_at. Called from the heartbeat handler
// so the reaper can distinguish live agents (with recent heartbeats) from
// zombies whose TCP stream is still open but the process is hung.
func (s *Store) MarkAgentSeen(ctx context.Context, agentID uuid.UUID) error {
	if err := s.q.UpdateAgentLastSeen(ctx, pgUUID(agentID)); err != nil {
		return fmt.Errorf("store: mark agent seen: %w", err)
	}
	return nil
}

// ReclaimStaleJobs walks every running job whose agent is offline or quiet
// beyond `staleness`, and either re-queues (when attempt < maxAttempts) or
// fails (via CompleteJob, so stage/run cascade still kicks in).
//
// Returns one entry per acted-on job; errors are attached per-entry so a
// single bad row doesn't abort the whole sweep.
func (s *Store) ReclaimStaleJobs(ctx context.Context, maxAttempts int32, staleness time.Duration) ([]ReclaimResult, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if staleness <= 0 {
		staleness = 90 * time.Second
	}

	stale, err := s.q.ListStaleRunningJobs(ctx, intervalFor(staleness))
	if err != nil {
		return nil, fmt.Errorf("store: list stale jobs: %w", err)
	}

	out := make([]ReclaimResult, 0, len(stale))
	for _, j := range stale {
		res := ReclaimResult{
			JobRunID:               fromPgUUID(j.ID),
			RunID:                  fromPgUUID(j.RunID),
			JobName:                j.Name,
			AgentID:                fromPgUUID(j.AgentID),
			AgentSessionGeneration: j.AgentSessionGeneration,
			Attempt:                j.Attempt,
		}

		if j.Attempt+1 > maxAttempts {
			// Cap reached — snapshot-validating fail. Stage/run cascade
			// runs inside failJobIfStale so the result matches the
			// existing CompleteJob terminal path exactly.
			_, ok, err := s.failJobIfStale(ctx, res.JobRunID, j.Attempt, res.AgentID,
				fmt.Sprintf("agent lost before completion (attempts=%d, max=%d)", j.Attempt, maxAttempts))
			switch {
			case err != nil:
				res.Err = fmt.Errorf("fail at max: %w", err)
			case !ok:
				res.Action = ReclaimActionSkipped
			default:
				res.Action = ReclaimActionFailed
			}
			out = append(out, res)
			continue
		}

		// Under the cap — atomic re-queue with snapshot validation.
		// Reaper path: notify=false. The caller (Reaper.Sweep) walks
		// the returned results, revokes the previous agent's live
		// session via the SessionFencer, THEN emits one coalesced
		// NotifyRunQueued per unique run id. Notifying in-tx here
		// would race: the scheduler can wake on the NOTIFY and
		// redispatch the just-requeued job onto the SAME stale
		// session (which still has capacity in memory) before the
		// fence kills it. RecordAssignmentCAS in DispatchAssignment
		// is the secondary trip-wire; doing the fence ordering at
		// the orchestration level is the primary defense.
		if err := s.requeueStaleJob(ctx, res.JobRunID, maxAttempts,
			j.Attempt, res.AgentID, false /*notify*/, &res); err != nil {
			res.Err = err
		}
		out = append(out, res)
	}
	return out, nil
}

// ReclaimAgentJobs is the register-fence path: declare every job
// currently attributed to `agentID` and still 'running' as orphaned,
// then reclaim each via the same requeue-or-fail-at-max logic the
// reaper uses for stale agents.
//
// Why a separate entry-point instead of waiting for the reaper:
//
// When an agent process restarts (k8s pod restart, OOM, crash + supervisor
// retry), the new process re-Registers with the SAME agent_id. MarkAgentOnline
// then sets agents.last_seen_at=NOW(). The reaper's stale-agent path keys
// off (status='offline' OR last_seen_at < NOW()-staleness), so the freshly-
// re-registered agent looks healthy and any job_runs still marked 'running'
// from the previous process incarnation remain forever — they have no
// observer, but the row says "running on agent A" and agent A is online.
// Combined with serial-concurrency gating that's an indefinite block on
// the affected pipeline.
//
// The fence solves it by treating "agent re-registered" as the explicit
// liveness signal: registration is a singular event per process lifetime
// (the agent client does NOT auto-reconnect — see agent/internal/rpc/
// client.go's Run comment), so anything attributed to that agent right
// before the new Register is by definition gone with the prior process.
//
// Race analysis (vs handleJobResult landing during the fence):
//   - reclaim path uses `WHERE status='running'` predicates; if a result
//     already flipped the row to terminal, ReclaimJobForRetry returns
//     ErrNoRows → marked Skipped. No double-action.
//   - if reclaim wins, the agent's result later arrives at a row whose
//     status is now 'queued'. CompleteJob's own predicate filters it
//     out. The result is silently dropped — acceptable: the agent is
//     ALSO dead by that point (its session was revoked by the new
//     Register), so the result wouldn't have travelled anyway.
//
// Scope: only this agent's rows. Other agents' running jobs are
// untouched. That's load-bearing in a multi-agent fleet — a fence
// stampede across the cluster would defeat the purpose.
func (s *Store) ReclaimAgentJobs(ctx context.Context, agentID uuid.UUID, maxAttempts int32) ([]ReclaimResult, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	rows, err := s.q.ListRunningJobsForAgent(ctx, pgUUID(agentID))
	if err != nil {
		return nil, fmt.Errorf("store: list running jobs for agent: %w", err)
	}
	if len(rows) == 0 {
		// Hot path: every register hits this. Avoid allocating an empty
		// slice for the common "no orphans" case.
		return nil, nil
	}

	out := make([]ReclaimResult, 0, len(rows))
	for _, j := range rows {
		res := ReclaimResult{
			JobRunID: fromPgUUID(j.ID),
			RunID:    fromPgUUID(j.RunID),
			JobName:  j.Name,
			AgentID:  fromPgUUID(j.AgentID),
			Attempt:  j.Attempt,
		}
		if j.Attempt+1 > maxAttempts {
			_, ok, err := s.failJobIfStale(ctx, res.JobRunID, j.Attempt, res.AgentID,
				fmt.Sprintf("agent re-registered before completion (attempts=%d, max=%d)", j.Attempt, maxAttempts))
			switch {
			case err != nil:
				res.Err = fmt.Errorf("fail at max: %w", err)
			case !ok:
				res.Action = ReclaimActionSkipped
			default:
				res.Action = ReclaimActionFailed
			}
			out = append(out, res)
			continue
		}
		// Fence path: notify=false. The OLD session (still in the
		// SessionStore until CreateSession revokes it) would otherwise
		// be eligible to pick up the just-requeued job in the gap
		// between fence completion and session swap — re-creating the
		// orphan we just cleaned up. The agent_service.go Register
		// handler emits one coalesced notify AFTER the new session is
		// in place, so the scheduler still gets woken exactly once.
		if err := s.requeueStaleJob(ctx, res.JobRunID, maxAttempts,
			j.Attempt, res.AgentID, false /*notify*/, &res); err != nil {
			res.Err = err
		}
		out = append(out, res)
	}
	return out, nil
}

// requeueStaleJob requeues a single stale job with snapshot validation
// — the (agent_id, attempt) we observed when listing the job MUST still
// match, otherwise a concurrent rerun / redispatch already changed the
// row out from under us and the requeue would clobber a healthy job
// running on a different agent. The validation is enforced atomically
// inside ReclaimJobForRetry.
//
// `notify` toggles the pg_notify(RunQueuedChannel, run_id) fire.
// Both production callers (register-fence in agent_service.go and
// reaper sweep in scheduler.Reaper) pass false — they revoke the
// previous agent's live session FIRST, then emit one coalesced
// NotifyRunQueued per requeued run. Without that ordering, the
// scheduler's LISTEN wake-up would race ahead and redispatch the
// just-requeued job onto the still-alive stale session before the
// fence killed it (HIGH bug fixed in round 10). The parameter
// stays for test ergonomics and future single-step callers.
func (s *Store) requeueStaleJob(
	ctx context.Context,
	jobID uuid.UUID,
	maxAttempts, expectedAttempt int32,
	expectedAgentID uuid.UUID,
	notify bool,
	res *ReclaimResult,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.ReclaimJobForRetry(ctx, db.ReclaimJobForRetryParams{
		ID:              pgUUID(jobID),
		MaxAttempts:     maxAttempts,
		ExpectedAttempt: expectedAttempt,
		ExpectedAgentID: pgUUIDNullable(expectedAgentID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either another sweeper tick won, the status flipped
			// out from under us, OR the (agent_id, attempt) snapshot
			// we observed at list time no longer matches (concurrent
			// rerun / redispatch). All three cases collapse to
			// "someone else owns this row now — leave it alone".
			res.Action = ReclaimActionSkipped
			return nil
		}
		return fmt.Errorf("reclaim: %w", err)
	}

	if err := q.DeleteLogLinesByJob(ctx, row.ID); err != nil {
		return fmt.Errorf("delete logs: %w", err)
	}
	// test_results is delete+reinsert per job_run_id (see
	// WriteTestResults). If a prior attempt left a JUnit report,
	// the Tests tab would surface those stale rows for the retry
	// — confusing the operator if the new attempt fails before
	// emitting a report. Match the log-line semantics: the row
	// is starting clean.
	if err := q.DeleteTestResultsByJobRun(ctx, row.ID); err != nil {
		return fmt.Errorf("delete test results: %w", err)
	}
	// Artifacts get the same treatment, but as a soft-delete so the
	// sweeper can clean up the underlying storage objects on its next
	// pass (issue #3 — without this, a reaper requeue left the prior
	// attempt's artifacts as 'ready', producing duplicate rows for
	// the same path the moment the new attempt re-uploaded, AND
	// pointing downstream consumers at storage about to be swept).
	// Partial-unique-index in migration 00035 makes the new attempt's
	// insert fail if this retire is missed, so the constraint catches
	// the bug at the schema layer too.
	if err := q.RetireArtifactsByJobRun(ctx, row.ID); err != nil {
		return fmt.Errorf("retire artifacts: %w", err)
	}
	// Coverage, same as logs/tests/artifacts above: a prior attempt's report is
	// keyed by job_run_id (UNIQUE), so without this the retry inherits stale
	// coverage until it emits its own — and a job that stops emitting coverage
	// keeps the old number forever.
	if err := q.DeleteCoverageReportsByJobRun(ctx, row.ID); err != nil {
		return fmt.Errorf("delete coverage: %w", err)
	}
	if notify {
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", RunQueuedChannel, fromPgUUID(row.RunID).String()); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	res.Action = ReclaimActionRequeued
	res.Attempt = row.Attempt
	return nil
}

// pgUUIDNullable maps uuid.Nil → invalid pgtype.UUID so the
// `IS NOT DISTINCT FROM` predicate matches NULL columns. Non-nil
// UUIDs round-trip as-is. Used by the snapshot path so NULL-agent
// reclaims (defensive secondary-defense category) compare correctly.
func pgUUIDNullable(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{Valid: false}
	}
	return pgUUID(id)
}

// failJobIfStale is the cap-exceeded twin of requeueStaleJob. Uses
// the dedicated FailStaleJobAtMax query (snapshot-validating Compare-
// And-Set) so a concurrent rerun / redispatch that moved the row to
// a healthy state on another agent doesn't get clobbered. Wraps the
// CAS + cascade in one transaction so stage/run promotion matches
// the existing CompleteJob path byte-for-byte.
//
// Returns ok=false when the snapshot no longer matches; caller marks
// the entry Skipped.
func (s *Store) failJobIfStale(
	ctx context.Context,
	jobID uuid.UUID,
	expectedAttempt int32,
	expectedAgentID uuid.UUID,
	reason string,
) (JobCompletion, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobCompletion{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.FailStaleJobAtMax(ctx, db.FailStaleJobAtMaxParams{
		ID:              pgUUID(jobID),
		ExpectedAttempt: expectedAttempt,
		ExpectedAgentID: pgUUIDNullable(expectedAgentID),
		Reason:          reason,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobCompletion{}, false, nil
		}
		return JobCompletion{}, false, fmt.Errorf("fail stale: %w", err)
	}

	comp := JobCompletion{
		JobRunID:   fromPgUUID(row.ID),
		RunID:      fromPgUUID(row.RunID),
		StageRunID: fromPgUUID(row.StageRunID),
		AgentID:    fromPgUUID(row.AgentID),
		JobName:    row.Name,
		StartedAt:  pgTimePtr(row.StartedAt),
		FinishedAt: pgTimePtr(row.FinishedAt),
	}
	if err := cascadeAfterJobCompletion(ctx, q, row.StageRunID, row.RunID, &comp); err != nil {
		return JobCompletion{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return JobCompletion{}, false, fmt.Errorf("commit: %w", err)
	}
	return comp, true, nil
}

func intervalFor(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
