package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// SessionFencer is the in-memory fencing hook the Reaper uses to
// revoke a stale agent's live session after its jobs are requeued.
//
// Without this, the scheduler — which picks agents purely by
// in-memory SessionStore capacity, never consulting agents.status —
// would happily redispatch the just-requeued job onto the SAME
// stale session that still has capacity in memory. DispatchAssignment
// overwrites the recorded (jobID → attempt) entry; a late JobResult
// from the old attempt then matches the new row's snapshot CAS
// and completes attempt N+1 with attempt N's payload.
//
// FenceStaleSession takes the agent's session generation as observed
// at SELECT time. If a successor Register has since bumped the
// generation (new healthy session in place), the fence is a no-op.
// Without this CAS, a perfectly-healthy successor could be revoked
// just because its predecessor was stale.
//
// The grpcsrv.FenceResult return distinguishes the three outcomes
// (revoked / no-session / generation-changed) so the sweep log can
// tell operators apart "stale process cleanly already gone" from
// "successor raced ahead". Correctness doesn't depend on the
// differentiation; operational signal does.
//
// SessionStore implements this interface; the production wiring in
// main.go injects it via WithSessionFencer.
type SessionFencer interface {
	FenceStaleSession(agentID uuid.UUID, observedGeneration int64) grpcsrv.FenceResult
}

// Reaper periodically sweeps running jobs whose agent is gone and either
// re-queues them (attempt < cap) or fails them via CompleteJob (at cap).
// Kept separate from Scheduler.Run because the concerns don't overlap:
// Scheduler dispatches queued work onto live agents; Reaper keeps the
// DB state honest about which of those "running" rows are actually alive.
type Reaper struct {
	store       *store.Store
	log         *slog.Logger
	interval    time.Duration
	staleness   time.Duration
	maxAttempts int32
	fencer      SessionFencer
}

// ReaperDefaults match the agent heartbeat cadence (30s): a tick every 30s
// gives us two missed heartbeats of grace before re-queueing, and a cap of
// 3 attempts prevents infinite retry loops on a job that crashes agents.
const (
	DefaultReaperInterval    = 30 * time.Second
	DefaultReaperStaleness   = 90 * time.Second
	DefaultReaperMaxAttempts = 3
)

// NewReaper constructs a Reaper with sensible defaults. Use the With*
// setters to tune for tests.
func NewReaper(s *store.Store, log *slog.Logger) *Reaper {
	if log == nil {
		log = slog.Default()
	}
	return &Reaper{
		store:       s,
		log:         log,
		interval:    DefaultReaperInterval,
		staleness:   DefaultReaperStaleness,
		maxAttempts: DefaultReaperMaxAttempts,
	}
}

// WithInterval / WithStaleness / WithMaxAttempts let tests compress the
// cadence without fiddling with internal fields directly.
func (r *Reaper) WithInterval(d time.Duration) *Reaper {
	if d > 0 {
		r.interval = d
	}
	return r
}

func (r *Reaper) WithStaleness(d time.Duration) *Reaper {
	if d > 0 {
		r.staleness = d
	}
	return r
}

func (r *Reaper) WithMaxAttempts(n int32) *Reaper {
	if n > 0 {
		r.maxAttempts = n
	}
	return r
}

// WithSessionFencer injects the in-memory session fencer used to
// revoke stale agents after their jobs are requeued. Production
// wires SessionStore here; tests that don't care about fencing
// can pass nil (Sweep just skips the revoke step).
func (r *Reaper) WithSessionFencer(f SessionFencer) *Reaper {
	r.fencer = f
	return r
}

// Run blocks until ctx is canceled, ticking every `interval` and sweeping
// stale jobs on each tick.
func (r *Reaper) Run(ctx context.Context) error {
	r.log.Info("reaper started",
		"interval", r.interval,
		"staleness", r.staleness,
		"max_attempts", r.maxAttempts)

	// Run once on startup to catch anything that was running when the server
	// last died — otherwise those jobs wait out a full interval tick.
	r.Sweep(ctx)

	t := time.NewTicker(r.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("reaper stopping")
			return nil
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep runs one pass. Exposed so tests (and admin endpoints later) can
// trigger it deterministically.
//
// Three-phase ordering — load-bearing for correctness:
//
//  1. ReclaimStaleJobs (notify=false internally): atomically requeues
//     each stale row with snapshot CAS; the row's (agent_id, attempt)
//     gets bumped but no LISTEN/NOTIFY fires.
//
//  2. RevokeForAgent on each unique previous_agent: kills the in-
//     memory session whose stream was attributed to that agent.
//     MUST happen BEFORE notify. If we notified first, the scheduler
//     would wake on the next LISTEN tick, FindIdle would still see
//     the stale session (which has spare capacity since its jobs
//     were just requeued out from under it), and DispatchAssignment
//     would reassign the same job to the same session — overwriting
//     its (jobID → attempt) record with the new attempt and turning
//     the late JobResult from the OLD attempt into a snapshot-CAS
//     match for the NEW row.
//
//  3. NotifyRunQueued per unique run_id: one coalesced wake-up after
//     the fence is complete. By this point the scheduler can't pick
//     the dead session.
//
// DispatchAssignment's RecordAssignmentCAS is the secondary trip-wire
// — if a stale session somehow survives phase 2 (test setup, future
// regression, fencer wired as nil), the per-attempt CAS refuses to
// overwrite a different-attempt assignment and the dispatch is
// rejected as busy.
func (r *Reaper) Sweep(ctx context.Context) {
	// Phase 0: finalise cancels deferred to the reaper. Jobs whose
	// operator clicked Cancel but whose Dispatch landed in the
	// Revoke→Register race window had cancel_requested_at stamped
	// in the DB; the agent's next Register replays them. If the
	// agent stays offline past `staleness`, the replay never fires
	// — we finalise the row here as 'canceled' so the UI stops
	// showing 'canceling' indefinitely. MUST run BEFORE
	// ReclaimStaleJobs: that path would requeue/fail-at-max the
	// same row by its own predicate, masking the operator's
	// cancel intent as a generic reaper requeue.
	if cancels, err := r.store.ReclaimPendingCancelsForOfflineAgent(ctx, r.staleness); err != nil {
		r.log.Warn("reaper: reclaim pending cancels failed", "err", err)
	} else if len(cancels) > 0 {
		notifySeen := make(map[uuid.UUID]struct{}, len(cancels))
		for _, c := range cancels {
			r.log.Info("reaper: pending cancel finalised",
				"run_id", c.RunID, "job_run_id", c.JobRunID,
				"agent_id", c.AgentID, "requested_at", c.CancelRequestedAt)
			if _, dup := notifySeen[c.RunID]; !dup {
				notifySeen[c.RunID] = struct{}{}
				if err := r.store.NotifyRunQueued(ctx, c.RunID); err != nil {
					r.log.Warn("reaper: pending cancel notify failed",
						"run_id", c.RunID, "err", err)
				}
			}
		}
	}

	results, err := r.store.ReclaimStaleJobs(ctx, r.maxAttempts, r.staleness)
	if err != nil {
		r.log.Warn("reaper: sweep failed", "err", err)
		return
	}
	if len(results) == 0 {
		return
	}

	// Walk once to compute counters AND deduplicated fence/notify
	// targets. Two maps keep us O(N) with cheap membership checks
	// — the iteration counts are small (one tick's stale rows),
	// but the dedup is functional, not perf: fencing the same
	// agent twice would no-op on the second call (latestByAg lookup
	// would miss after the first Revoke), and notifying the same
	// run id N times pumps useless wake-ups onto the channel.
	//
	// For fence dedup we key on agentID alone, NOT (agent, generation):
	// every stale row from agent X observed in this snapshot
	// references the SAME `agents.session_generation` value — the
	// counter is per-agent, not per-job. Two rows can't disagree on
	// it within one SELECT.
	type fenceTarget struct {
		agentID    uuid.UUID
		generation int64
	}
	var requeued, failed, skipped, errored int
	var fenceTargets []fenceTarget
	var notifyRuns []uuid.UUID
	seenAgents := make(map[uuid.UUID]struct{}, len(results))
	seenRuns := make(map[uuid.UUID]struct{}, len(results))
	addFenceTarget := func(res store.ReclaimResult) {
		if res.AgentID == uuid.Nil {
			return
		}
		if _, dup := seenAgents[res.AgentID]; dup {
			return
		}
		seenAgents[res.AgentID] = struct{}{}
		fenceTargets = append(fenceTargets, fenceTarget{
			agentID:    res.AgentID,
			generation: res.AgentSessionGeneration,
		})
	}
	for _, res := range results {
		switch {
		case res.Err != nil:
			errored++
			r.log.Warn("reaper: reclaim entry error",
				"job_id", res.JobRunID, "err", res.Err)
		case res.Action == store.ReclaimActionRequeued:
			requeued++
			r.log.Info("reaper: job re-queued",
				"run_id", res.RunID, "job_id", res.JobRunID,
				"job_name", res.JobName, "attempt", res.Attempt,
				"previous_agent", res.AgentID,
				"previous_generation", res.AgentSessionGeneration)
			addFenceTarget(res)
			if _, dup := seenRuns[res.RunID]; !dup {
				seenRuns[res.RunID] = struct{}{}
				notifyRuns = append(notifyRuns, res.RunID)
			}
		case res.Action == store.ReclaimActionFailed:
			failed++
			r.log.Warn("reaper: job failed at max attempts",
				"run_id", res.RunID, "job_id", res.JobRunID,
				"job_name", res.JobName, "attempts", res.Attempt+1)
			// FailStaleJobAtMax cascades into stage/run terminal,
			// nothing to notify on. But the agent IS still stale —
			// fence the session so a subsequent register cycle
			// gets a clean slate without a phantom-capacity entry
			// lingering in the SessionStore.
			addFenceTarget(res)
		default:
			skipped++
		}
	}

	// Phase 2: revoke in-memory sessions for affected agents — but
	// only when the live session's generation still matches the one
	// we observed at SELECT time. A successor Register that bumped
	// the counter between SELECT and now means the live session is
	// a NEW healthy one; fencing it would close a clean stream and
	// (since FenceStaleSession does NOT set supersededByRegister)
	// trigger an offline mark that would then race the next register.
	//
	// fencer is nil in tests that don't exercise this path; that's
	// fine — the per-row notify is gated below and the snapshot
	// CAS at the DB still prevents log/result corruption.
	var fenced, fenceNoSession, fenceGenChanged int
	if r.fencer != nil {
		for _, t := range fenceTargets {
			switch r.fencer.FenceStaleSession(t.agentID, t.generation) {
			case grpcsrv.FenceResultRevoked:
				fenced++
			case grpcsrv.FenceResultNoSession:
				fenceNoSession++
				r.log.Debug("reaper: fence no-op — no live session for agent (already disconnected)",
					"agent_id", t.agentID, "observed_generation", t.generation)
			case grpcsrv.FenceResultGenerationChanged:
				fenceGenChanged++
				r.log.Info("reaper: fence skipped — generation changed (successor register raced ahead)",
					"agent_id", t.agentID, "observed_generation", t.generation)
			}
		}
	}

	// Phase 3: coalesced wake-ups, one per unique run.
	for _, runID := range notifyRuns {
		if err := r.store.NotifyRunQueued(ctx, runID); err != nil {
			r.log.Warn("reaper: notify failed", "run_id", runID, "err", err)
		}
	}

	r.log.Info("reaper: sweep done",
		"requeued", requeued, "failed", failed, "skipped", skipped, "errors", errored,
		"fence_targets", len(fenceTargets), "fenced", fenced,
		"fence_no_session", fenceNoSession,
		"fence_skipped_generation_changed", fenceGenChanged,
		"notified_runs", len(notifyRuns))
}
