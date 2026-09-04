package scheduler

import (
	"context"
	"time"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// FireRunTerminalEffects fires the generic post-terminal side effects for a run.
// Exported for tests; the live path is driven by LISTEN/NOTIFY plus replay.
func (s *Scheduler) FireRunTerminalEffects(ctx context.Context, runID uuid.UUID) {
	s.fireRunTerminalEffects(ctx, runID)
}

func (s *Scheduler) fireRunTerminalEffects(ctx context.Context, runID uuid.UUID) {
	effectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	claim, claimed, err := s.store.ClaimRunTerminalEffects(effectCtx, runID)
	if err != nil {
		s.log.Warn("run terminal effects: claim", "run_id", runID, "err", err)
		return
	}
	if !claimed {
		return
	}

	// GitHub completion is intentionally best-effort. The reporter re-reads the
	// run's current status under its own advisory lock, so a stale claim racing a
	// rerun skips instead of closing a reopened check.
	if s.checks != nil {
		s.checks.ReportRunCompleted(effectCtx, runID, claim.Status)
	}

	if !s.cleanupTerminalRunServices(effectCtx, runID, claim.ServiceGeneration) {
		return
	}
	done, err := s.store.MarkRunTerminalEffectsDone(effectCtx, runID, claim.ServiceGeneration)
	if err != nil {
		s.log.Warn("run terminal effects: mark done", "run_id", runID, "err", err)
		return
	}
	if !done {
		s.log.Info("run terminal effects: stale claim skipped by generation/status guard",
			"run_id", runID, "service_generation", claim.ServiceGeneration)
	}
}

func (s *Scheduler) replayRunTerminalEffects(ctx context.Context) {
	ids, err := s.store.ListPendingRunTerminalEffects(ctx, 100)
	if err != nil {
		s.log.Warn("run terminal effects: replay list", "err", err)
		return
	}
	for _, id := range ids {
		s.fireRunTerminalEffects(ctx, id)
	}
}

func (s *Scheduler) cleanupTerminalRunServices(ctx context.Context, runID uuid.UUID, maxGeneration int64) bool {
	hasServices, err := s.store.RunHasServices(ctx, runID)
	if err != nil {
		s.log.Warn("run terminal effects: has-services check failed; broadcasting cleanup anyway",
			"run_id", runID, "err", err)
		hasServices = true
	}
	if !hasServices {
		return true
	}
	if s.sessions == nil {
		s.log.Warn("run terminal effects: no session store; service cleanup will retry",
			"run_id", runID)
		return false
	}
	ran, err := s.store.ListAgentsForRun(ctx, runID)
	if err != nil {
		s.log.Warn("run terminal effects: list agents for cleanup; continuing with connected-only",
			"run_id", runID, "err", err)
	}
	seen := make(map[uuid.UUID]struct{}, len(ran))
	targets := make([]uuid.UUID, 0, len(ran))
	for _, id := range ran {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}
	for _, id := range s.sessions.AllAgentIDs("kubernetes") {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		s.log.Warn("run terminal effects: cleanup has no target yet; will retry via replay",
			"run_id", runID)
		return false
	}

	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_CleanupRunServices{
			CleanupRunServices: &gocdnextv1.CleanupRunServices{
				RunId:         runID.String(),
				MaxGeneration: maxGeneration,
			},
		},
	}
	delivered := false
	for _, id := range targets {
		if err := s.sessions.Dispatch(id, msg); err != nil {
			s.log.Warn("run terminal effects: cleanup dispatch failed",
				"run_id", runID, "agent_id", id, "err", err)
			continue
		}
		delivered = true
	}
	if !delivered {
		s.log.Warn("run terminal effects: cleanup reached no agent; will retry via replay",
			"run_id", runID)
	}
	return delivered
}
