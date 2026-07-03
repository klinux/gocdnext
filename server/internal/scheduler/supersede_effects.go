package scheduler

import (
	"context"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// FireSupersedeEffects fires the external side effects of a supersede-cancel that
// the store deliberately left to the caller (SupersededRunChannel NOTIFY): push a
// CancelJob frame to every still-running job of the superseded run so its
// container stops promptly instead of waiting for the agent's next reconnect, then
// broadcast a service cleanup if the run declared services. Exported so tests can
// drive it without the LISTEN loop; the live path calls it from Run's NOTIFY case.
//
// Best-effort throughout: the store already flipped the run to canceled +
// superseded_by and stamped cancel_requested_at on the running jobs (that's the
// durable terminal state). These effects only make the stop PROMPT; a dispatch
// failure (agent mid-reconnect, scheduler restart missed the NOTIFY) degrades to
// the reconnect-replay + reaper paths, not to a stuck run.
func (s *Scheduler) FireSupersedeEffects(ctx context.Context, runID uuid.UUID) {
	s.fireSupersedeEffects(ctx, runID)
}

func (s *Scheduler) fireSupersedeEffects(ctx context.Context, runID uuid.UUID) {
	jobs, err := s.store.ListRunningCancelRequestedForRun(ctx, runID)
	if err != nil {
		s.log.Warn("supersede effects: list running jobs", "run_id", runID, "err", err)
		// fall through — still attempt service cleanup below.
	}
	for _, j := range jobs {
		msg := &gocdnextv1.ServerMessage{
			Kind: &gocdnextv1.ServerMessage_Cancel{
				Cancel: &gocdnextv1.CancelJob{
					RunId:  runID.String(),
					JobId:  j.JobID.String(),
					Reason: "superseded",
				},
			},
		}
		if err := s.sessions.Dispatch(j.AgentID, msg); err != nil {
			s.log.Warn("supersede effects: cancel dispatch failed; cancel_requested_at stamp finalizes on reconnect",
				"run_id", runID, "job_id", j.JobID, "agent_id", j.AgentID, "err", err)
		}
	}
	s.cleanupSupersededServices(ctx, runID)
}

// cleanupSupersededServices broadcasts CleanupRunServices to (agents that ran a
// job of this run) ∪ (connected k8s agents) when the run declared services, so a
// superseded run's `services:` pods don't leak (a gate-pending victim is usually
// service-less, hence the cheap RunHasServices gate first). Mirrors the same
// broadcast the API cancel handler and the AgentService run-terminal cascade do —
// duplicated rather than shared because each holds a different session handle
// (SessionStore vs the api dispatcher interface); a future refactor could unify.
func (s *Scheduler) cleanupSupersededServices(ctx context.Context, runID uuid.UUID) {
	hasServices, err := s.store.RunHasServices(ctx, runID)
	if err != nil {
		// Fail-open: one extra empty List beats leaking pods.
		s.log.Warn("supersede effects: has-services check failed; broadcasting cleanup anyway", "run_id", runID, "err", err)
		hasServices = true
	}
	if !hasServices {
		return
	}
	ran, err := s.store.ListAgentsForRun(ctx, runID)
	if err != nil {
		s.log.Warn("supersede effects: list agents for cleanup; continuing with connected-only", "run_id", runID, "err", err)
	}
	seen := make(map[uuid.UUID]struct{}, len(ran))
	targets := make([]uuid.UUID, 0, len(ran))
	for _, id := range ran {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			targets = append(targets, id)
		}
	}
	for _, id := range s.sessions.AllAgentIDs("kubernetes") {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		s.log.Warn("supersede effects: cleanup has no targets; pods may leak until manual cleanup", "run_id", runID)
		return
	}
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_CleanupRunServices{
			CleanupRunServices: &gocdnextv1.CleanupRunServices{RunId: runID.String()},
		},
	}
	for _, id := range targets {
		if err := s.sessions.Dispatch(id, msg); err != nil {
			s.log.Warn("supersede effects: cleanup dispatch failed", "run_id", runID, "agent_id", id, "err", err)
		}
	}
}
