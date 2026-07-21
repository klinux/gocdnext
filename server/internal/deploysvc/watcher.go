package deploysvc

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DeployProvider is the provider capability the watcher drives: one convergence
// snapshot for a target, plus (Phase 2 gate control) Promote/Abort of the canary.
// Promote/Abort act on the target's PINNED Rollout identity; both are idempotent.
// *deploy.ArgoProvider satisfies it.
type DeployProvider interface {
	Observe(ctx context.Context, target deploy.DeploymentTarget) (deploy.DeployState, error)
	Promote(ctx context.Context, target deploy.DeploymentTarget) error
	Abort(ctx context.Context, target deploy.DeploymentTarget) error
}

// WatchStore is the persistence the watcher drives. *store.Store satisfies it. Every
// mutation past the claim is fenced on the claim token; a false return means the
// lease was lost (reclaimed by another replica) and this watcher must drop the watch.
type WatchStore interface {
	ClaimDeployWatches(ctx context.Context, claimedBy string, leaseSeconds, max int) ([]store.DeployWatch, error)
	RenewDeployWatch(ctx context.Context, revID, claimID uuid.UUID) (bool, error)
	SetDeployWatchDegradedSince(ctx context.Context, revID, claimID uuid.UUID) (bool, error)
	ClearDeployWatchDegraded(ctx context.Context, revID, claimID uuid.UUID) (bool, error)
	StampRolloutObservation(ctx context.Context, revID, claimID uuid.UUID, in store.RolloutObservationInput) (bool, error)
	FinalizeDeployWatch(ctx context.Context, revID, claimID uuid.UUID, status, reason string) (store.DeployWatchFinalizeResult, error)
	NotifyRunQueued(ctx context.Context, runID uuid.UUID) error

	// Gate control (Phase 2), all fenced on the claim token.
	ArmRolloutGate(ctx context.Context, revID, claimID uuid.UUID, in store.ArmRolloutGateInput) (bool, error)
	MarkGateActioned(ctx context.Context, revID, claimID uuid.UUID) (bool, error)
	ClearRolloutGate(ctx context.Context, revID, claimID uuid.UUID) (bool, error)
	// MarkRolloutAbortActioned is the cancel/supersede abort (stamp + disarm + delete
	// votes). DeployWatchCancelRequestedAt reads the deploy's cancel intent each tick.
	MarkRolloutAbortActioned(ctx context.Context, revID, claimID uuid.UUID) (bool, error)
	DeployWatchCancelRequestedAt(ctx context.Context, revID uuid.UUID) (*time.Time, error)
}

// Watcher cadence/lease defaults. The lease TTL must comfortably exceed one watch's
// processing budget (an Observe is bounded by the cluster API timeout) so a watch
// processed late in a batch still holds its lease.
const (
	DefaultWatchInterval     = 5 * time.Second
	DefaultWatchLeaseSeconds = 60
	DefaultWatchBatch        = 20
	DefaultDegradedWindow    = 2 * time.Minute
)

// Watcher is the stateful deploy-watch driver (ADR-0001, Inc.6b). Each tick it claims
// a batch of claimable deploy_watches and, per watch IN ISOLATION, renews the lease,
// observes the Application, Decides, and applies the single resulting effect.
//
// Contracts (all fenced on the claim token):
//   - the lease is renewed before the long I/O (Observe) and again before a terminal
//     write, so a tick running near the TTL never acts on an about-to-be-reclaimed lease;
//   - an Observe error NEVER finalizes — it logs and retries next tick until the
//     deadline (which Decide enforces once observation resumes);
//   - FinalizeDeployWatch is the ONLY terminal path;
//   - any fenced false (renew/degraded/finalize) drops the watch immediately;
//   - one watch's error or panic never stalls the batch.
//
// Prometheus is deferred; the structured watch_* log events are the stable surface.
type Watcher struct {
	obs   DeployProvider
	store WatchStore
	log   *slog.Logger

	workerID       string
	interval       time.Duration
	leaseSeconds   int
	batch          int
	degradedWindow time.Duration
}

// NewWatcher builds a watcher for workerID (the lease holder identity — use a stable
// per-replica id so a restart reclaims its own leases cleanly).
func NewWatcher(obs DeployProvider, s WatchStore, workerID string, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		obs:            obs,
		store:          s,
		log:            log,
		workerID:       workerID,
		interval:       DefaultWatchInterval,
		leaseSeconds:   DefaultWatchLeaseSeconds,
		batch:          DefaultWatchBatch,
		degradedWindow: DefaultDegradedWindow,
	}
}

// With* let tests compress the cadence / window without touching internals.
func (w *Watcher) WithInterval(d time.Duration) *Watcher {
	if d > 0 {
		w.interval = d
	}
	return w
}

func (w *Watcher) WithLeaseSeconds(n int) *Watcher {
	if n > 0 {
		w.leaseSeconds = n
	}
	return w
}

func (w *Watcher) WithBatch(n int) *Watcher {
	if n > 0 {
		w.batch = n
	}
	return w
}

func (w *Watcher) WithDegradedWindow(d time.Duration) *Watcher {
	if d > 0 {
		w.degradedWindow = d
	}
	return w
}

// Run blocks until ctx is canceled, ticking every interval.
func (w *Watcher) Run(ctx context.Context) error {
	w.log.Info("deploy watcher started",
		"worker_id", w.workerID, "interval", w.interval,
		"lease_seconds", w.leaseSeconds, "batch", w.batch, "degraded_window", w.degradedWindow)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("deploy watcher stopping")
			return nil
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one pass: claim a batch and process each watch in isolation. Exposed so
// tests (and, later, an admin trigger) can drive it deterministically.
func (w *Watcher) Tick(ctx context.Context) {
	watches, err := w.store.ClaimDeployWatches(ctx, w.workerID, w.leaseSeconds, w.batch)
	if err != nil {
		w.log.Error("watch_error", "phase", "claim", "err", err)
		return
	}
	for _, dw := range watches {
		if ctx.Err() != nil {
			return
		}
		w.log.Debug("watch_claimed", watchAttrs(dw)...)
		w.processWatch(ctx, dw)
	}
}

// processWatch handles one claimed watch. It never propagates: an error or panic in a
// single watch must not stall the rest of the batch.
func (w *Watcher) processWatch(ctx context.Context, dw store.DeployWatch) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("watch_error", append(watchAttrs(dw), "phase", "panic", "recover", r)...)
		}
	}()

	// Renew BEFORE the long I/O. A fenced false = lease lost → drop.
	if ok, err := w.store.RenewDeployWatch(ctx, dw.DeploymentRevisionID, dw.ClaimID); err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "renew", "err", err)...)
		return
	} else if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "renew")...)
		return
	}

	// Read the deploy's cancel/supersede intent this tick. STICKY: once a cancel-abort was
	// issued (rollout_abort_actioned_at stamped — which only happens after an observed
	// cancel), the deploy is being canceled, so keep it in Decide's cancel branch even if
	// this tick's cancel read transiently fails. Otherwise a read error would drop
	// CancelRequested to false and a Synced+Healthy+FullyPromoted snapshot could
	// FinalizeSuccess a canceled deploy. A fresh read only ADDS the intent.
	cancelRequested := dw.RolloutAbortActionedAt != nil
	if at, err := w.store.DeployWatchCancelRequestedAt(ctx, dw.DeploymentRevisionID); err != nil {
		w.log.Warn("watch_error", append(watchAttrs(dw), "phase", "cancel_read", "err", err)...)
	} else if at != nil {
		cancelRequested = true
	}

	state, err := w.obs.Observe(ctx, targetOf(dw))
	if err != nil {
		// An Application read error never TRUSTS the returned snapshot — but it must
		// still run through Decide, or the Phase-2 precedence is lost when a gate is
		// armed: a human REJECT (or cancel) must still Abort via the pin (abort-safe
		// under uncertainty), an armed & undecided gate must still SUSPEND the deadline,
		// and only APPROVE stays promote-unsafe. So replace the state with a SANITIZED
		// synthetic one — no untrusted Sync/Health/rev (a stale value must not drive a
		// success/degraded/fast-fail), only the observe-error signal — persist it for the
		// UI, and fall through to Decide. For a non-rollout / observe-only deploy this
		// yields exactly the old behavior: Continue, or FinalizeFailed(deadline) once the
		// budget is spent (Decide branch 4, since awaitingHuman is false there). The
		// reason is FIXED (the raw err may carry the internal API-server URL).
		w.log.Warn("watch_error", append(watchAttrs(dw), "phase", "observe", "err", err)...)
		state = deploy.DeployState{RolloutError: "the deploy could not be observed"}
		if dw.RolloutAware {
			w.persistRollout(ctx, dw, store.RolloutObservationInput{Error: state.RolloutError})
		}
	} else {
		w.log.Debug("watch_observed", append(watchAttrs(dw),
			"sync", state.Sync, "health", state.Health, "observed_rev", state.ObservedRev, "op_phase", state.OperationPhase)...)
		// Persist the observed Rollout snapshot each tick so the UI (which reads the DB)
		// renders canary progress. The same live snapshot (state.Rollout) also feeds
		// Decide below — arm/promote/abort/clear + the no-early-finalize guard.
		if dw.RolloutAware {
			w.stampRollout(ctx, dw, state)
		}
	}

	verdict := deploy.Decide(state, anchorsOf(dw, cancelRequested), time.Now(), w.degradedWindow)
	w.log.Info("watch_decision", append(watchAttrs(dw), "effect", verdict.Effect, "reason", verdict.Reason)...)

	switch verdict.Effect {
	case deploy.Continue:
		// keep watching — no state change this tick.
	case deploy.SetDegraded:
		w.applyDegraded(ctx, dw, "set", w.store.SetDeployWatchDegradedSince)
	case deploy.ClearDegraded:
		w.applyDegraded(ctx, dw, "clear", w.store.ClearDeployWatchDegraded)
	case deploy.ArmGate:
		w.armGate(ctx, dw, state)
	case deploy.Promote:
		w.actuateGate(ctx, dw, deploy.Promote)
	case deploy.Abort:
		// Route on the DB-derived cancel intent, NOT the verdict reason: a cancel/supersede
		// abort disarms the gate + marks the gate-independent guard; a reject abort marks
		// the gated action. (Decide's cancel branch outranks reject, so these agree.)
		if cancelRequested {
			w.abortForCancel(ctx, dw, state)
		} else {
			w.actuateGate(ctx, dw, deploy.Abort)
		}
	case deploy.ClearGate:
		w.clearGate(ctx, dw)
	case deploy.FinalizeSuccess:
		w.finalize(ctx, dw, store.DeployStatusSuccess, "")
	case deploy.FinalizeFailed:
		w.finalize(ctx, dw, store.DeployStatusFailed, verdict.Reason)
	}
}

func (w *Watcher) applyDegraded(ctx context.Context, dw store.DeployWatch, kind string,
	fn func(context.Context, uuid.UUID, uuid.UUID) (bool, error)) {
	ok, err := fn(ctx, dw.DeploymentRevisionID, dw.ClaimID)
	if err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "degraded_"+kind, "err", err)...)
		return
	}
	if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "degraded_"+kind)...)
	}
}

func (w *Watcher) finalize(ctx context.Context, dw store.DeployWatch, status, reason string) {
	// Renew right before the terminal write so a tick that ran close to the TTL
	// doesn't act on an about-to-be-reclaimed lease. (FinalizeDeployWatch is itself
	// fenced, so this only turns a lost lease into an earlier, cleaner drop.)
	if ok, err := w.store.RenewDeployWatch(ctx, dw.DeploymentRevisionID, dw.ClaimID); err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "renew_finalize", "err", err)...)
		return
	} else if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "renew_finalize")...)
		return
	}
	res, err := w.store.FinalizeDeployWatch(ctx, dw.DeploymentRevisionID, dw.ClaimID, status, reason)
	if err != nil {
		w.log.Error("watch_error", append(watchAttrs(dw), "phase", "finalize", "err", err)...)
		return
	}
	if !res.Finalized {
		// Lease lost, or the job/reaper path already terminalized this revision (its
		// cleanup deleted the watch). Either way we're done with it.
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "finalize")...)
		return
	}
	w.log.Info("watch_finalize", append(watchAttrs(dw), "status", status, "reason", reason)...)
	// Post-commit effect: nudge the run so the scheduler advances the next stage
	// promptly (the DB cascade already committed; this is a latency optimization —
	// idempotent, best-effort, the scheduler's periodic tick would catch it anyway).
	if res.RunID != uuid.Nil {
		if err := w.store.NotifyRunQueued(ctx, res.RunID); err != nil {
			w.log.Warn("watch_error", append(watchAttrs(dw), "phase", "notify", "err", err)...)
		}
	}
}

// stampRollout persists the observed Rollout snapshot for a successful Observe.
func (w *Watcher) stampRollout(ctx context.Context, dw store.DeployWatch, state deploy.DeployState) {
	in := store.RolloutObservationInput{Error: state.RolloutError}
	if state.RolloutObserved {
		r := state.Rollout
		in = store.RolloutObservationInput{
			Observed:    true,
			Phase:       string(r.Phase),
			Message:     r.Message,
			PauseReason: r.PauseReason,
			CurrentStep: r.CurrentStepIndex,
			StepKnown:   r.CurrentStepKnown,
			StepCount:   r.StepCount,
			Aborted:     r.Aborted,
		}
	}
	w.persistRollout(ctx, dw, in)
}

// persistRollout is the fenced, best-effort write of a rollout snapshot (a lost lease
// or error just logs — the UI snapshot is best-effort).
func (w *Watcher) persistRollout(ctx context.Context, dw store.DeployWatch, in store.RolloutObservationInput) {
	if ok, err := w.store.StampRolloutObservation(ctx, dw.DeploymentRevisionID, dw.ClaimID, in); err != nil {
		w.log.Warn("watch_error", append(watchAttrs(dw), "phase", "rollout_observe", "err", err)...)
	} else if !ok {
		w.log.Info("watch_lease_lost", append(watchAttrs(dw), "phase", "rollout_observe")...)
	}
}

func targetOf(dw store.DeployWatch) deploy.DeploymentTarget {
	return deploy.DeploymentTarget{
		ProjectID:        dw.ProjectID,
		Provider:         "argocd",
		Cluster:          dw.Cluster,
		Application:      dw.Application,
		Namespace:        dw.Namespace,
		SyncMode:         deploy.SyncMode(dw.SyncMode),
		RolloutAware:     dw.RolloutAware,
		RolloutCluster:   dw.RolloutCluster,
		RolloutNamespace: dw.RolloutNamespace,
		RolloutName:      dw.RolloutName,
	}
}

func anchorsOf(dw store.DeployWatch, cancelRequested bool) deploy.WatchAnchors {
	return deploy.WatchAnchors{
		SyncMode:         deploy.SyncMode(dw.SyncMode),
		ExpectedRevision: dw.ExpectedRevision,
		SyncRequestedAt:  dw.SyncRequestedAt,
		DeadlineAt:       dw.DeadlineAt,
		DegradedSince:    dw.DegradedSince,

		// Gate control (Phase 2).
		RolloutAware:    dw.RolloutAware,
		Gated:           dw.Gated,
		CancelRequested: cancelRequested,
		GateArmedAt:     dw.GateArmedAt,
		GateDecision:    deploy.GateDecision(dw.GateDecision),
		GateActionedAt:  dw.GateActionedAt,
		GatePausedStep:  gatePausedStepPtr(dw),
		// A COMPLETE pinned tuple — a partial pin is not actionable (the store rejects
		// arming with one), so it must not read as "pinned".
		HasPinnedRolloutTarget: dw.GateRolloutCluster != "" && dw.GateRolloutNamespace != "" && dw.GateRolloutName != "",
		RolloutAbortActionedAt: dw.RolloutAbortActionedAt,
	}
}

func watchAttrs(dw store.DeployWatch) []any {
	return []any{
		"revision_id", dw.DeploymentRevisionID,
		"claim_id", dw.ClaimID,
		"sync_mode", dw.SyncMode,
		"cluster", dw.Cluster,
		"application", dw.Application,
	}
}
