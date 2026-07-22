package deploysvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DefaultDeployDeadline is the convergence budget for a native deploy — past it the
// watcher fails the deploy (progress deadline exceeded).
const DefaultDeployDeadline = 15 * time.Minute

// Syncer actuates a target toward a revision (a no-op for observe mode).
// *deploy.ArgoProvider satisfies it.
type Syncer interface {
	Sync(ctx context.Context, target deploy.DeploymentTarget, revision string) error
}

// NativeStore is the persistence the takeover drives. *store.Store satisfies it.
type NativeStore interface {
	ResolveDeployTarget(ctx context.Context, projectID uuid.UUID, env string) (store.DeployTarget, error)
	StartNativeDeploy(ctx context.Context, in store.StartNativeDeployInput) (store.StartNativeDeployResult, error)
	// StartNativeDeployDeclared is the same takeover, but it locks the target row and
	// asserts the pipeline's declaration still holds INSIDE the tx — a gate added in
	// the window must not yield an ungated deploy.
	StartNativeDeployDeclared(ctx context.Context, in store.StartNativeDeployInput, want store.DeclaredTargetExpectation) (store.StartNativeDeployResult, store.DeclaredTakeoverOutcome, error)
	StampDeployWatchSyncRequested(ctx context.Context, revID uuid.UUID) (bool, error)
}

// NativeDecision is the takeover verdict the scheduler branches on.
type NativeDecision string

const (
	// DecisionNative: the native provider took over — the job is server-managed; do
	// NOT dispatch it to an agent.
	DecisionNative NativeDecision = "native"
	// DecisionFallback: no deploy_target registered for the env — use the plugin path.
	DecisionFallback NativeDecision = "fallback"
	// DecisionGated: a DECLARED takeover found the target gated under the lock. Terminal
	// — the pipeline must drop `deploy.target`, or an admin must ungate it.
	DecisionGated NativeDecision = "gated"
	// DecisionSkip: the dispatch CAS was lost (another tick/replica won). Do nothing;
	// the job is no longer this tick's to act on.
	DecisionSkip NativeDecision = "skip"
)

// NativeDeployInput is the scheduler's dispatch-time request.
// DeclaredExpectation mirrors the pipeline's `deploy.target` for the guarded takeover.
// nil => the target is whatever was registered out-of-band (the pre-existing path).
type DeclaredExpectation = store.DeclaredTargetExpectation

type NativeDeployInput struct {
	// Declared, when set, makes the takeover assert the declaration on a LOCKED target
	// row before creating the revision/watch.
	Declared    *DeclaredExpectation
	ProjectID   uuid.UUID
	RunID       uuid.UUID
	JobRunID    uuid.UUID
	Environment string
	// Version is the human-facing deploy version stored in deployment_revisions and
	// shown in the UI (e.g. a semver or the commit short sha).
	Version string
	// Revision is the git revision ArgoCD is expected to report in
	// .status.sync.revision / operationState.syncResult.revision — the FULL commit
	// SHA. It is what the watch correlates + Evaluate matches for success, so it must
	// be the full SHA (NOT the short-sha display Version, which would never match).
	// Empty ("") leaves the watch unpinned (success on any Synced+Healthy).
	Revision   string
	DeployedBy string
	Now        time.Time // dispatch time; the convergence deadline is Now + deadline
}

// NativeDeployResult tells the scheduler what happened (and carries fields for its
// audit log on the native path).
type NativeDeployResult struct {
	Decision    NativeDecision
	RevisionID  uuid.UUID
	Attempt     int32
	Application string
	SyncMode    string
}

// NativeDeployer orchestrates the native deploy takeover (ADR-0001, Model A): resolve
// the target, atomically start the server-managed deploy (revision + watch + running
// job), then trigger the sync and stamp the correlation anchor. Orchestration lives
// here, not in the scheduler.
type NativeDeployer struct {
	sync  Syncer
	store NativeStore
	log   *slog.Logger
	// registrar backs ReconcileDeclarativeTarget. Optional: nil disables
	// pipeline-declared targets loudly (see WithRegistrar).
	registrar *Registrar
	deadline  time.Duration
}

func NewNativeDeployer(sync Syncer, s NativeStore, log *slog.Logger) *NativeDeployer {
	if log == nil {
		log = slog.Default()
	}
	return &NativeDeployer{sync: sync, store: s, log: log, deadline: DefaultDeployDeadline}
}

// WithRegistrar wires the declarative-target reconcile. Kept explicit rather than
// derived from the store so the dependency is visible at the call site; nil means a
// pipeline that declares `deploy.target` fails loud instead of silently ignoring the
// declaration and deploying against whatever happens to be registered.
func (d *NativeDeployer) WithRegistrar(r *Registrar) *NativeDeployer {
	d.registrar = r
	return d
}

// ReconcileDeclarativeTarget delegates to the Registrar, which owns the single-snapshot
// read/decide/write. NativeDeployer implements it so the scheduler keeps ONE seam for
// "everything native", instead of learning about two collaborators.
func (d *NativeDeployer) ReconcileDeclarativeTarget(ctx context.Context, in DeclarativeReconcileInput) (DeclarativeResult, error) {
	if d.registrar == nil {
		return DeclarativeResult{}, errors.New("deploysvc: declarative deploy targets are not configured on this server")
	}
	return d.registrar.ReconcileDeclarativeTarget(ctx, in)
}

// WithDeadline overrides the convergence budget (tests compress it).
func (d *NativeDeployer) WithDeadline(dur time.Duration) *NativeDeployer {
	if dur > 0 {
		d.deadline = dur
	}
	return d
}

// HasTarget reports whether the environment has a registered deploy target — i.e.
// whether TakeOver would take the native path. The scheduler asks this BEFORE applying
// any native-only validation (notably the git-SHA correlation requirement), because a
// tracking-layer deploy legitimately carries any version string (an image tag, a
// semver) and must never be failed by a rule that doesn't apply to it. A real
// registry/DB failure is returned as an error so the caller fails closed.
func (d *NativeDeployer) HasTarget(ctx context.Context, projectID uuid.UUID, environment string) (bool, error) {
	if _, err := d.store.ResolveDeployTarget(ctx, projectID, environment); err != nil {
		if errors.Is(err, store.ErrDeployTargetNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("deploysvc: resolve deploy target: %w", err)
	}
	return true, nil
}

// TakeOver decides and (on the native path) performs the takeover. It returns an
// error ONLY on a fail-closed condition (a real registry/DB failure) — the scheduler
// must then NOT dispatch and retry later. A missing target is not an error
// (DecisionFallback); a lost CAS is not an error (DecisionSkip).
func (d *NativeDeployer) TakeOver(ctx context.Context, in NativeDeployInput) (NativeDeployResult, error) {
	tgt, err := d.store.ResolveDeployTarget(ctx, in.ProjectID, in.Environment)
	if err != nil {
		if errors.Is(err, store.ErrDeployTargetNotFound) {
			return NativeDeployResult{Decision: DecisionFallback}, nil
		}
		// A real registry/infra error: fail closed — never silently fall to the plugin.
		return NativeDeployResult{}, fmt.Errorf("deploysvc: resolve deploy target: %w", err)
	}

	startIn := store.StartNativeDeployInput{
		JobRunID:         in.JobRunID,
		EnvironmentID:    tgt.EnvironmentID,
		RunID:            in.RunID,
		Version:          in.Version,
		DeployedBy:       in.DeployedBy,
		ProjectID:        in.ProjectID,
		SyncMode:         tgt.SyncMode,
		Cluster:          tgt.Cluster,
		Application:      tgt.Application,
		Namespace:        tgt.Namespace,
		ExpectedRevision: in.Revision, // full SHA for correlation, NOT the display Version
		DeadlineAt:       in.Now.Add(d.deadline),
		RolloutAware:     tgt.RolloutAware,
		RolloutCluster:   tgt.RolloutCluster,
		RolloutNamespace: tgt.RolloutNamespace,
		RolloutName:      tgt.RolloutName,
		GoverningGate:    tgt.GoverningGate,
	}

	var res store.StartNativeDeployResult
	if in.Declared != nil {
		// Guarded takeover: the declaration is asserted on a LOCKED target row inside
		// the same tx that creates the revision + watch. Checking it out here would be
		// check-then-act across a transaction boundary — a gate inserted in the gap
		// would still produce an UNGATED deploy, since StartNativeDeploy never re-reads
		// deploy_targets.
		var outcome store.DeclaredTakeoverOutcome
		res, outcome, err = d.store.StartNativeDeployDeclared(ctx, startIn, *in.Declared)
		if err != nil {
			return NativeDeployResult{}, fmt.Errorf("deploysvc: start declared native deploy: %w", err)
		}
		switch outcome {
		case store.DeclaredTakeoverGated:
			// Terminal: never start an ungated deploy of a target an admin just gated.
			return NativeDeployResult{Decision: DecisionGated}, nil
		case store.DeclaredTakeoverDrifted:
			// Benign race with a concurrent edit — the next tick's reconcile settles it.
			return NativeDeployResult{Decision: DecisionSkip}, nil
		}
	} else {
		res, err = d.store.StartNativeDeploy(ctx, startIn)
		if err != nil {
			return NativeDeployResult{}, fmt.Errorf("deploysvc: start native deploy: %w", err) // fail-closed
		}
	}
	if !res.Started {
		return NativeDeployResult{Decision: DecisionSkip}, nil // lost the CAS
	}

	d.log.Info("deploy_native_selected",
		"environment", in.Environment, "application", tgt.Application, "sync_mode", tgt.SyncMode,
		"revision_id", res.RevisionID, "run_id", in.RunID, "job_run_id", in.JobRunID)

	// Trigger the sync + stamp the correlation anchor — trigger mode only. Observe
	// mode issues no sync (the watcher observes an auto-synced app), so it never stamps.
	if tgt.SyncMode == string(deploy.SyncModeTrigger) {
		syncTarget := deploy.DeploymentTarget{
			ProjectID: in.ProjectID, Provider: tgt.Provider, Cluster: tgt.Cluster,
			Application: tgt.Application, Namespace: tgt.Namespace, SyncMode: deploy.SyncMode(tgt.SyncMode),
		}
		if err := d.sync.Sync(ctx, syncTarget, in.Revision); err != nil {
			// Conservative: do NOT complete the job here. Leaving sync_requested_at NULL,
			// the watcher deadline-fails and completes job + revision together — a single
			// terminalizer. The takeover still happened (running job owned by the watch).
			d.log.Warn("deploy_native_sync_failed",
				"revision_id", res.RevisionID, "environment", in.Environment,
				"application", tgt.Application, "err", err)
		} else if ok, err := d.store.StampDeployWatchSyncRequested(ctx, res.RevisionID); err != nil {
			d.log.Warn("deploy_native_stamp_failed", "revision_id", res.RevisionID, "err", err)
		} else if !ok {
			d.log.Info("deploy_native_stamp_noop", "revision_id", res.RevisionID)
		}
	}

	return NativeDeployResult{
		Decision: DecisionNative, RevisionID: res.RevisionID, Attempt: res.Attempt,
		Application: tgt.Application, SyncMode: tgt.SyncMode,
	}, nil
}
