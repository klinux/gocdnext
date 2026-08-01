package scheduler

import (
	"context"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// External side effects of an approval-gate expiry, split out of
// approval_expirer.go for the same reason supersede_effects.go is split from
// the supersede path: the decision logic (which gate expired, and why) and the
// fan-out that stops the work (agent frames, pod teardown) are separate
// concerns that change for separate reasons.
//
// Everything here is BEST-EFFORT, on the same footing as the API cancel path —
// deliberately NOT supersede's durable claim/lease/replay. The durable state
// (run canceled, gate stamped `expired`) is already committed by the time any of
// this runs, so nothing here can leave a run stuck.
//
// Known gap, stated plainly rather than implied away: a missed CancelJob frame
// is recovered by the agent reconnect-replay and the reaper, but a services
// teardown that reaches NO connected k8s agent is not retried, so pods can leak
// until a manual sweep. supersede_effects.go withholds its done-marker until
// cleanup resolves precisely to close that hole; matching it here needs the same
// claim/lease columns on a table that has none.
//
// The exposure is narrower than supersede's: a supersede fires the instant a
// newer run appears, routinely catching a victim mid-build with live service
// pods, whereas expiry fires only after a window elapses. Under the 7-day
// default there is nothing still running to leak. The gap is real for a gate
// whose window is short enough to expire while its run's jobs still execute
// (`timeout: 5m` on a late-stage gate) — which is why the failure is logged at
// WARN naming the run, instead of being silently swallowed.

// fireCancelEffects pushes CancelJob frames to jobs still executing inside the
// canceled run and broadcasts the service teardown — the same two effects the
// API cancel handler fires, because the DB flip alone leaves containers and
// `services:` pods burning until they finish naturally.
//
// A run parked at a gate is NOT necessarily idle: gates are armed at run
// creation, so a stage-0 build can still be executing while a later gate times
// out. Best-effort throughout — the durable state is already committed, and a
// missed frame degrades to the agent-reconnect replay and the reaper.
func (e *ApprovalExpirer) fireCancelEffects(ctx context.Context, runID uuid.UUID, res store.ExpireApprovalGateResult) {
	if e.dispatcher == nil {
		return
	}
	for _, j := range res.RunningJobs {
		msg := &gocdnextv1.ServerMessage{
			Kind: &gocdnextv1.ServerMessage_Cancel{
				Cancel: &gocdnextv1.CancelJob{
					RunId:  runID.String(),
					JobId:  j.JobID.String(),
					Reason: "approval timeout",
				},
			},
		}
		if err := e.dispatcher.Dispatch(j.AgentID, msg); err != nil {
			e.log.Warn("approval expirer: cancel dispatch failed; finalises on reconnect",
				"run_id", runID, "job_id", j.JobID, "agent_id", j.AgentID, "err", err)
		}
	}
	e.cleanupServices(ctx, runID, res.ServiceGeneration)
}

// cleanupServices broadcasts CleanupRunServices to (agents that ran a job of
// this run) ∪ (connected k8s agents), so a canceled run's `services:` pods
// don't leak. Mirrors the API cancel handler's broadcast; duplicated rather
// than shared because each caller holds a different session handle.
//
// maxGeneration comes from the cancel UPDATE itself, captured atomically with
// the flip to canceled (#97) — so a rerun that later revives this run into a
// higher generation keeps its fresh pods instead of having them swept by this
// stale cleanup.
func (e *ApprovalExpirer) cleanupServices(ctx context.Context, runID uuid.UUID, maxGeneration int64) {
	// Cheap gate first: a pipeline with no `services:` has no pods to clean,
	// and a gate-parked run usually has none. Fail-open on error — one extra
	// empty List beats leaking pods.
	hasServices, err := e.store.RunHasServices(ctx, runID)
	if err != nil {
		e.log.Warn("approval expirer: has-services check failed; broadcasting anyway",
			"run_id", runID, "err", err)
		hasServices = true
	}
	if !hasServices {
		return
	}
	ran, err := e.store.ListAgentsForRun(ctx, runID)
	if err != nil {
		e.log.Warn("approval expirer: list agents for cleanup; continuing with connected-only",
			"run_id", runID, "err", err)
	}
	seen := make(map[uuid.UUID]struct{}, len(ran))
	targets := make([]uuid.UUID, 0, len(ran))
	for _, id := range ran {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			targets = append(targets, id)
		}
	}
	for _, id := range e.dispatcher.AllAgentIDs("kubernetes") {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		e.log.Warn("approval expirer: cleanup has no target; pods may leak until manual cleanup",
			"run_id", runID)
		return
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
		if err := e.dispatcher.Dispatch(id, msg); err != nil {
			e.log.Debug("approval expirer: cleanup dispatch failed",
				"run_id", runID, "agent_id", id, "err", err)
			continue
		}
		delivered = true
	}
	if !delivered {
		e.log.Warn("approval expirer: cleanup reached no agent; pods may leak until manual cleanup",
			"run_id", runID)
	}
}
