package scheduler

import (
	"context"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
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
	// Claim first so exactly one worker fires the effects: a second replica's
	// listener, or the periodic replay, gets claimed=false and backs off. A claim
	// past its lease (the prior claimer crashed mid-effects) is reclaimable, so no
	// effects are permanently lost. MarkSupersedeEffectsDone at the end ends the
	// retry loop — the idempotent audit + naturally-idempotent frames/cleanup/check
	// keep a lease-expiry retry safe.
	claimed, err := s.store.ClaimSupersedeEffects(ctx, runID)
	if err != nil {
		s.log.Warn("supersede effects: claim", "run_id", runID, "err", err)
		return
	}
	if !claimed {
		return // another live claim owns it, or the effects already completed
	}

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
	// DURABLE effects gate the done-marker: cleanup (pods must actually be reachable)
	// and audit (a DB write). If either can't resolve now — the classic case is a run
	// with services when no k8s agent is connected yet — we do NOT mark done, so the
	// lease-expiry replay retries until it resolves. Both are idempotent, so a retry
	// that re-runs an already-succeeded one is harmless.
	cleanupResolved := s.cleanupSupersededServices(ctx, runID)
	auditResolved := s.emitSupersedeAudit(ctx, runID)

	// Close the run's GitHub check. Supersede terminalizes straight to canceled,
	// skipping the JobResult path that normally reports completion — without this a
	// check created for the old run stays in_progress forever. Nil-safe + re-reads
	// the run's current status (canceled → conclusion=cancelled). Explicitly
	// BEST-EFFORT (fire-and-forget GitHub PATCH): it does NOT gate the done-marker —
	// coupling internal retry to GitHub uptime isn't worth it, and a stale
	// in_progress check is cosmetic vs. leaked pods. Re-fired (idempotently) on each
	// retry that a durable effect triggers, so it gets extra chances anyway.
	if s.checks != nil {
		s.checks.ReportRunCompleted(ctx, runID, string(domain.StatusCanceled))
	}

	// Mark done only when the durable effects truly resolved (or didn't apply).
	// Otherwise leave effects_at NULL — the replay reclaims after the lease and
	// retries (e.g. once a k8s agent reconnects).
	if cleanupResolved && auditResolved {
		if err := s.store.MarkSupersedeEffectsDone(ctx, runID); err != nil {
			s.log.Warn("supersede effects: mark done", "run_id", runID, "err", err)
		}
	}
}

// replaySupersedeEffects re-fires effects for superseded runs the NOTIFY path
// missed (channel-full drop, scheduler restart) or a claimer abandoned past its
// lease. Runs on the tick alongside drainQueued; each run re-enters
// fireSupersedeEffects which re-claims under the lease, so a healthy in-flight claim
// is left alone. Bounded per tick.
func (s *Scheduler) replaySupersedeEffects(ctx context.Context) {
	ids, err := s.store.ListPendingSupersedeEffects(ctx, 100)
	if err != nil {
		s.log.Warn("supersede effects: replay list", "err", err)
		return
	}
	for _, id := range ids {
		s.fireSupersedeEffects(ctx, id)
	}
}

// emitSupersedeAudit records the run.superseded audit once per victim, unified here
// so BOTH fire points (creation + cascade) get identical audit off the same NOTIFY.
// Counters + the superseding run id only — never a branch/ref value. Returns
// whether the audit RESOLVED (written, or a clean ON CONFLICT no-op, or nothing to
// record) — false only on a transient error worth a replay retry.
func (s *Scheduler) emitSupersedeAudit(ctx context.Context, runID uuid.UUID) bool {
	info, ok, err := s.store.SupersededAuditInfo(ctx, runID)
	if err != nil {
		s.log.Warn("supersede effects: audit info", "run_id", runID, "err", err)
		return false // transient read error — retry
	}
	if !ok {
		return true // not superseded — nothing to record, don't block done
	}
	// Idempotent (partial unique index): a replica race or a lease-expiry replay
	// re-running this is a clean no-op.
	if err := s.store.EmitRunSupersededAudit(ctx, runID, map[string]any{
		"superseded_counter": info.SupersededCounter,
		"by_run_id":          info.BySupersedingRunID.String(),
		"by_counter":         info.ByCounter,
	}); err != nil {
		s.log.Warn("supersede effects: audit emit", "run_id", runID, "err", err)
		return false
	}
	return true
}

// cleanupSupersededServices broadcasts CleanupRunServices to (agents that ran a
// job of this run) ∪ (connected k8s agents) when the run declared services, so a
// superseded run's `services:` pods don't leak (a gate-pending victim is usually
// service-less, hence the cheap RunHasServices gate first). Mirrors the same
// broadcast the API cancel handler and the AgentService run-terminal cascade do —
// duplicated rather than shared because each holds a different session handle
// (SessionStore vs the api dispatcher interface); a future refactor could unify.
//
// Returns whether the cleanup RESOLVED: true when the run has no services (nothing
// to clean) or the broadcast reached at least one k8s agent; false when the run HAS
// services but no agent could receive the frame yet — the durability case the replay
// must retry (else a run whose pods outlive every connected agent leaks until a
// manual sweep). CleanupRunServices is cluster-scoped + idempotent, so reaching any
// one k8s agent is sufficient, and a retry after more reconnect is harmless.
func (s *Scheduler) cleanupSupersededServices(ctx context.Context, runID uuid.UUID) bool {
	hasServices, err := s.store.RunHasServices(ctx, runID)
	if err != nil {
		// Fail-open: one extra empty List beats leaking pods.
		s.log.Warn("supersede effects: has-services check failed; broadcasting cleanup anyway", "run_id", runID, "err", err)
		hasServices = true
	}
	if !hasServices {
		return true // nothing to clean
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
		s.log.Warn("supersede effects: cleanup has no target yet; will retry via replay", "run_id", runID)
		return false // has services but no receiver — retry when an agent reconnects
	}
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_CleanupRunServices{
			CleanupRunServices: &gocdnextv1.CleanupRunServices{RunId: runID.String()},
		},
	}
	delivered := false
	for _, id := range targets {
		if err := s.sessions.Dispatch(id, msg); err != nil {
			s.log.Warn("supersede effects: cleanup dispatch failed", "run_id", runID, "agent_id", id, "err", err)
			continue
		}
		delivered = true
	}
	if !delivered {
		s.log.Warn("supersede effects: cleanup reached no agent (all dispatches failed); will retry via replay", "run_id", runID)
	}
	return delivered // resolved only if the frame reached at least one k8s agent
}
