package scheduler

import (
	"context"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// FireMergeGroupCancelEffects fires the prompt external effects of a
// merge_group.destroyed cancellation: stop still-running jobs and clean service
// pods. It intentionally does NOT report the run as canceled to GitHub; the
// merge-group SHA has already been abandoned and a red/cancelled status there is
// noise at best.
func (s *Scheduler) FireMergeGroupCancelEffects(ctx context.Context, runID uuid.UUID) {
	s.fireMergeGroupCancelEffects(ctx, runID)
}

func (s *Scheduler) fireMergeGroupCancelEffects(ctx context.Context, runID uuid.UUID) {
	claimed, _, err := s.store.ClaimMergeGroupCancelEffects(ctx, runID)
	if err != nil {
		s.log.Warn("merge_group cancel effects: claim", "run_id", runID, "err", err)
		return
	}
	if !claimed {
		return
	}

	jobs, err := s.store.ListRunningCancelRequestedForRun(ctx, runID)
	if err != nil {
		s.log.Warn("merge_group cancel effects: list running jobs", "run_id", runID, "err", err)
	}
	for _, j := range jobs {
		msg := &gocdnextv1.ServerMessage{
			Kind: &gocdnextv1.ServerMessage_Cancel{
				Cancel: &gocdnextv1.CancelJob{
					RunId:  runID.String(),
					JobId:  j.JobID.String(),
					Reason: "merge_group destroyed",
				},
			},
		}
		if err := s.sessions.Dispatch(j.AgentID, msg); err != nil {
			s.log.Warn("merge_group cancel effects: cancel dispatch failed; cancel_requested_at stamp finalizes on reconnect",
				"run_id", runID, "job_id", j.JobID, "agent_id", j.AgentID, "err", err)
		}
	}

	if s.cleanupMergeGroupCanceledServices(ctx, runID) {
		if err := s.store.MarkMergeGroupCancelEffectsDone(ctx, runID); err != nil {
			s.log.Warn("merge_group cancel effects: mark done", "run_id", runID, "err", err)
		}
	}
}

func (s *Scheduler) replayMergeGroupCancelEffects(ctx context.Context) {
	ids, err := s.store.ListPendingMergeGroupCancelEffects(ctx, 100)
	if err != nil {
		s.log.Warn("merge_group cancel effects: replay list", "err", err)
		return
	}
	for _, id := range ids {
		s.fireMergeGroupCancelEffects(ctx, id)
	}
}

func (s *Scheduler) cleanupMergeGroupCanceledServices(ctx context.Context, runID uuid.UUID) bool {
	generation, stillCanceled, err := s.store.MergeGroupCanceledRunServiceGeneration(ctx, runID)
	if err != nil {
		s.log.Warn("merge_group cancel effects: revive re-check failed; skipping cleanup this pass", "run_id", runID, "err", err)
		return false
	}
	if !stillCanceled {
		return true
	}
	hasServices, err := s.store.RunHasServices(ctx, runID)
	if err != nil {
		s.log.Warn("merge_group cancel effects: has-services check failed; broadcasting cleanup anyway", "run_id", runID, "err", err)
		hasServices = true
	}
	if !hasServices {
		return true
	}
	ran, err := s.store.ListAgentsForRun(ctx, runID)
	if err != nil {
		s.log.Warn("merge_group cancel effects: list agents for cleanup; continuing with connected-only", "run_id", runID, "err", err)
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
		s.log.Warn("merge_group cancel effects: cleanup has no target yet; will retry via replay", "run_id", runID)
		return false
	}
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_CleanupRunServices{
			CleanupRunServices: &gocdnextv1.CleanupRunServices{
				RunId:         runID.String(),
				MaxGeneration: generation,
			},
		},
	}
	delivered := false
	for _, id := range targets {
		if err := s.sessions.Dispatch(id, msg); err != nil {
			s.log.Warn("merge_group cancel effects: cleanup dispatch failed", "run_id", runID, "agent_id", id, "err", err)
			continue
		}
		delivered = true
	}
	if !delivered {
		s.log.Warn("merge_group cancel effects: cleanup reached no agent (all dispatches failed); will retry via replay", "run_id", runID)
	}
	return delivered
}
