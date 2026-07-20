package deploysvc

// Gate-driven Argo Rollouts control effects (ADR-0001 Phase 2). The watcher is the ONLY
// cluster actuator; these handlers apply the Decide machine's gate verdicts (ArmGate /
// Promote / Abort / ClearGate) against the store + provider, all fenced on the claim
// token and renewing right before any external Promote/Abort.

import (
	"context"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// armGate stamps the approval gate for this indefinite pause, pinning the identity
// Observe just resolved (ArmGate is only decided on a fresh observed pause, so the
// resolved cluster/namespace/name are present). Best-effort + fenced: a lost lease or an
// incomplete pin just logs — the next tick re-decides.
func (w *Watcher) armGate(ctx context.Context, dw store.DeployWatch, state deploy.DeployState) {
	r := state.Rollout
	ok, err := w.store.ArmRolloutGate(ctx, dw.DeploymentRevisionID, dw.ClaimID, store.ArmRolloutGateInput{
		PausedStep:       r.CurrentStepIndex,
		RolloutCluster:   r.ResolvedCluster,
		RolloutNamespace: r.ResolvedNamespace,
		RolloutName:      r.ResolvedName,
	})
	if err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "arm_gate", "err", err)...)
		return
	}
	if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "arm_gate")...)
		return
	}
	w.log.Info("watch_gate_armed", append(watchAttrs(dw), "paused_step", r.CurrentStepIndex,
		"rollout_cluster", r.ResolvedCluster, "rollout_namespace", r.ResolvedNamespace, "rollout_name", r.ResolvedName)...)
}

// actuateGate is the ONLY caller of Promote/Abort — the watcher is the single actuator.
// It renews the lease right before the external effect (per the single-actuator
// contract), acts on the gate-PINNED identity, and only then stamps gate_actioned_at. If
// the actuation fails, gate_actioned_at stays NULL so the next tick retries (Promote/
// Abort are idempotent); if the mark is fenced out, another replica re-actuates safely.
func (w *Watcher) actuateGate(ctx context.Context, dw store.DeployWatch, effect deploy.VerdictEffect) {
	phase := string(effect)
	// Renew right before the external effect so a tick near the TTL doesn't actuate on
	// an about-to-be-reclaimed lease.
	if ok, err := w.store.RenewDeployWatch(ctx, dw.DeploymentRevisionID, dw.ClaimID); err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "renew_"+phase, "err", err)...)
		return
	} else if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "renew_"+phase)...)
		return
	}

	target := pinnedTargetOf(dw)
	var actErr error
	if effect == deploy.Promote {
		actErr = w.obs.Promote(ctx, target)
	} else {
		actErr = w.obs.Abort(ctx, target)
	}
	if actErr != nil {
		// Leave gate_actioned_at NULL → the next tick re-decides + retries (idempotent).
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", phase, "err", actErr)...)
		return
	}
	w.log.Info("watch_gate_actuated", append(watchAttrs(dw), "effect", phase,
		"rollout_name", target.RolloutName)...)

	if ok, err := w.store.MarkGateActioned(ctx, dw.DeploymentRevisionID, dw.ClaimID); err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "mark_actioned", "err", err)...)
	} else if !ok {
		// Lease lost after a successful (idempotent) actuation — a reclaiming replica
		// re-actuates + marks. Not an error.
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "mark_actioned")...)
	}
}

// clearGate disarms the current step's gate (the rollout left the armed step, ours or
// external). Fenced + best-effort.
func (w *Watcher) clearGate(ctx context.Context, dw store.DeployWatch) {
	if ok, err := w.store.ClearRolloutGate(ctx, dw.DeploymentRevisionID, dw.ClaimID); err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "clear_gate", "err", err)...)
	} else if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "clear_gate")...)
	} else {
		w.log.Info("watch_gate_cleared", watchAttrs(dw)...)
	}
}

// pinnedTargetOf builds the target Promote/Abort act on: the App identity plus the
// gate-PINNED Rollout identity (gate_rollout_*), never the routing/auto-discovery. Decide
// only emits Promote/Abort when the gate is armed+pinned, so these are set.
func pinnedTargetOf(dw store.DeployWatch) deploy.DeploymentTarget {
	t := targetOf(dw)
	t.RolloutCluster = dw.GateRolloutCluster
	t.RolloutNamespace = dw.GateRolloutNamespace
	t.RolloutName = dw.GateRolloutName
	return t
}

// gatePausedStepPtr returns the armed step index, or nil when no step is recorded — so
// Decide's "still at the armed step" check treats an absent step as unknown (fail-closed
// toward clearing) rather than step 0.
func gatePausedStepPtr(dw store.DeployWatch) *int {
	if !dw.GatePausedStepKnown {
		return nil
	}
	s := dw.GatePausedStep
	return &s
}
