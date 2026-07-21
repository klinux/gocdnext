package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// appFetcher returns the raw ArgoCD Application resource JSON for a target. The
// transport lives behind this seam; tests inject fixtures.
type appFetcher interface {
	fetchApplication(ctx context.Context, target DeploymentTarget) ([]byte, error)
}

// rolloutFetcher returns the raw Argo Rollouts Rollout CR JSON on the workload's
// destination cluster (ADR-0001 Phase 2). Same test seam as appFetcher.
type rolloutFetcher interface {
	fetchRollout(ctx context.Context, projectID uuid.UUID, cluster, namespace, name string) ([]byte, error)
}

// appSyncer triggers a sync for a target (behind the same test seam as the fetcher).
// syncOptions carries the Application's own sync options so a manual sync honors them.
type appSyncer interface {
	syncApplication(ctx context.Context, target DeploymentTarget, revision string, syncOptions []string) error
}

// ArgoProvider is the ArgoCD-backed provider. It observes an Application and, for
// trigger-mode targets, actuates a sync by writing its `.operation` — never
// reconciling desired state itself (that is ArgoCD's job). For rollout-aware targets
// it also reads and (Phase 2) controls the Argo Rollouts canary via Promote/Abort. It
// satisfies the full DeploymentProvider interface (Sync + Observe + Promote + Abort).
type ArgoProvider struct {
	fetch   appFetcher
	rollout rolloutFetcher
	sync    appSyncer
	actuate rolloutActuator
}

// NewArgoProvider wires the provider over the store-backed k8s-CRD transport: the
// server passes its cluster registry (a ClusterAPI: read for Observe, patch for
// Sync + rollout promote/abort) and gets a provider that reads/actuates Applications
// and Rollouts through the target cluster's k8s API.
func NewArgoProvider(api ClusterAPI) *ArgoProvider {
	f := newK8sAppFetcher(api)
	return &ArgoProvider{
		fetch:   f,
		rollout: f,
		sync:    newK8sAppSyncer(api),
		actuate: newK8sRolloutActuator(api),
	}
}

// newArgoProviderWith injects an appFetcher directly — for tests that exercise
// Observe with a fixture fetcher, no cluster involved.
func newArgoProviderWith(f appFetcher) *ArgoProvider {
	return &ArgoProvider{fetch: f}
}

// newArgoProviderWithSync injects both seams for Sync tests.
func newArgoProviderWithSync(f appFetcher, s appSyncer) *ArgoProvider {
	return &ArgoProvider{fetch: f, sync: s}
}

// newArgoProviderWithActuator injects the rollout actuator — for Promote/Abort tests.
func newArgoProviderWithActuator(f appFetcher, act rolloutActuator) *ArgoProvider {
	return &ArgoProvider{fetch: f, actuate: act}
}

// Observe fetches the target's Application and returns one convergence snapshot.
// When the target is RolloutAware it ALSO resolves + reads the Rollout the
// Application manages and reports it in state.Rollout. A rollout-resolution failure
// does NOT fail Observe (the Application read succeeded) — it leaves
// RolloutObserved=false and records a short sanitized reason in state.RolloutError,
// which the watcher persists and (in control mode) treats as fail-closed. Only an
// Application fetch/parse failure returns an error.
func (a *ArgoProvider) Observe(ctx context.Context, target DeploymentTarget) (DeployState, error) {
	raw, err := a.fetch.fetchApplication(ctx, target)
	if err != nil {
		return DeployState{}, fmt.Errorf("deploy: fetch application %s/%s: %w", target.Namespace, target.Application, err)
	}
	state, err := parseApplicationStatus(raw)
	if err != nil {
		return DeployState{}, err
	}
	if target.RolloutAware {
		if rollout, reason := a.observeRollout(ctx, target, raw); reason == "" {
			state.Rollout, state.RolloutObserved = rollout, true
		} else {
			state.RolloutError = reason
		}
	}
	return state, nil
}

// observeRollout resolves the target's Rollout (explicit name/namespace, else
// group-filtered auto-discovery from the Application's `.status.resources[]`), reads
// it from the resolved cluster, and parses it. Returns ("" reason) on success, or a
// short SANITIZED reason on any failure — never the raw transport error (which can
// carry the internal API-server URL).
func (a *ArgoProvider) observeRollout(ctx context.Context, target DeploymentTarget, appRaw []byte) (RolloutState, string) {
	if a.rollout == nil {
		return RolloutState{}, "rollout transport not configured"
	}
	ns, name := target.RolloutNamespace, target.RolloutName
	if ns == "" || name == "" {
		dns, dname, derr := discoverRollout(appRaw)
		if derr != nil {
			return RolloutState{}, derr.Error() // ErrRolloutNotFound / ErrMultipleRollouts — safe messages
		}
		if ns == "" {
			ns = dns
		}
		if name == "" {
			name = dname
		}
	}
	cluster := target.RolloutCluster
	if cluster == "" {
		cluster = target.Cluster
	}
	raw, ferr := a.rollout.fetchRollout(ctx, target.ProjectID, cluster, ns, name)
	if ferr != nil {
		// SANITIZED: the raw transport error can carry the internal API-server URL,
		// so never surface it. The specific reason (404 vs transport) has no
		// behavioural effect (both fail closed in control mode), so one generic
		// message suffices for the UI.
		return RolloutState{}, "the rollout could not be read from the cluster"
	}
	st, perr := parseRolloutState(raw)
	if perr != nil {
		return RolloutState{}, "rollout status could not be parsed"
	}
	st.ResolvedCluster, st.ResolvedNamespace, st.ResolvedName = cluster, ns, name
	return st, ""
}

// Sync actuates the target toward revision by writing the Application's `.operation`.
// It is a no-op for observe-mode targets (gocdnext issues no sync — it only watches
// an auto-synced app). An empty revision syncs to the Application's targetRevision.
func (a *ArgoProvider) Sync(ctx context.Context, target DeploymentTarget, revision string) error {
	if target.SyncMode == SyncModeObserve {
		return nil
	}
	if a.sync == nil {
		return errors.New("deploy: provider has no syncer configured")
	}
	// Honor the Application's own sync options (CreateNamespace, ServerSideApply, …) on
	// the manual sync — a bare `.operation.sync` does NOT inherit spec.syncPolicy, so
	// without this an app relying on CreateNamespace fails its first deploy with
	// "namespace not found". This mirrors `argocd app sync` / the UI. Best-effort: if
	// the read fails we sync without them (the sync itself, or the watcher's deadline,
	// surfaces a genuine problem) rather than blocking the deploy on the extra GET.
	var syncOptions []string
	if a.fetch != nil {
		if raw, err := a.fetch.fetchApplication(ctx, target); err == nil {
			syncOptions = parseSyncOptions(raw)
		}
	}
	if err := a.sync.syncApplication(ctx, target, revision, syncOptions); err != nil {
		return fmt.Errorf("deploy: sync application %s/%s: %w", target.Namespace, target.Application, err)
	}
	return nil
}

// Promote advances a step-paused Argo Rollouts canary one step (clears its
// pauseConditions). It acts on the target's RESOLVED/pinned Rollout identity
// (RolloutCluster/Namespace/Name) — the watcher passes the gate-pinned identity, never
// a re-discovery — so drift in the Application's `.status.resources[]` between observe,
// vote, and act can't redirect the effect. Idempotent: a promote on an
// already-advanced rollout is a harmless no-op merge-patch.
func (a *ArgoProvider) Promote(ctx context.Context, target DeploymentTarget) error {
	if a.actuate == nil {
		return errors.New("deploy: provider has no rollout actuator configured")
	}
	if err := a.actuate.promoteRollout(ctx, target); err != nil {
		return fmt.Errorf("deploy: promote rollout %s/%s: %w", target.RolloutNamespace, target.RolloutName, err)
	}
	return nil
}

// Abort reverts an Argo Rollouts canary's TRAFFIC to the stable ReplicaSet (sets
// `.status.abort`). Like Promote it acts on the pinned identity. It does NOT revert
// Git/desired — `.spec.template` keeps the new version, so a re-sync or a corrected
// commit rolls forward. Idempotent.
func (a *ArgoProvider) Abort(ctx context.Context, target DeploymentTarget) error {
	if a.actuate == nil {
		return errors.New("deploy: provider has no rollout actuator configured")
	}
	if err := a.actuate.abortRollout(ctx, target); err != nil {
		return fmt.Errorf("deploy: abort rollout %s/%s: %w", target.RolloutNamespace, target.RolloutName, err)
	}
	return nil
}

// parseSyncOptions reads `.spec.syncPolicy.syncOptions` from a raw Application CR — the
// options a manual sync must carry to behave like the app's own (auto)sync. Nil on any
// decode error or when none are set.
func parseSyncOptions(raw []byte) []string {
	var app struct {
		Spec struct {
			SyncPolicy struct {
				SyncOptions []string `json:"syncOptions"`
			} `json:"syncPolicy"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil
	}
	return app.Spec.SyncPolicy.SyncOptions
}

// applicationStatus is the minimal slice of an ArgoCD Application resource this
// provider reads. Extra fields (and unknown enum values) are tolerated by design.
//
// Note on multi-source: multi-source Applications report `.status.sync.revisions`
// (a list) and leave `.status.sync.revision` empty. Multi-source is OUT OF SCOPE
// for this slice — the target registry rejects it — so we read only the
// single-source `.revision`. A multi-source app therefore yields ObservedRev="",
// which keeps the revision check fail-closed (Pending, never a false success).
type applicationStatus struct {
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState struct {
			Phase      string `json:"phase"`
			StartedAt  string `json:"startedAt"`
			SyncResult struct {
				Revision string `json:"revision"`
			} `json:"syncResult"`
		} `json:"operationState"`
	} `json:"status"`
}

func parseApplicationStatus(raw []byte) (DeployState, error) {
	var app applicationStatus
	if err := json.Unmarshal(raw, &app); err != nil {
		return DeployState{}, fmt.Errorf("deploy: decode application status: %w", err)
	}
	return DeployState{
		Sync:               normalizeSync(app.Status.Sync.Status),
		Health:             normalizeHealth(app.Status.Health.Status),
		ObservedRev:        app.Status.Sync.Revision,
		OperationPhase:     normalizeOpPhase(app.Status.OperationState.Phase),
		OperationStartedAt: parseK8sTime(app.Status.OperationState.StartedAt),
		SyncResultRevision: app.Status.OperationState.SyncResult.Revision,
	}, nil
}

// parseK8sTime parses an ArgoCD/Kubernetes RFC3339 timestamp (e.g.
// `.status.operationState.startedAt`). An absent or unparseable value yields the
// zero time — the watch loop treats that as "no reliable operation timestamp" and
// fails closed (won't correlate the operation to this deploy), never as a spurious
// match. A malformed timestamp must not drop the otherwise-valid observation, so
// this never errors.
func parseK8sTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func normalizeSync(s string) SyncStatus {
	switch SyncStatus(s) {
	case SyncSynced, SyncOutOfSync:
		return SyncStatus(s)
	default:
		return SyncUnknown
	}
}

func normalizeHealth(s string) HealthStatus {
	switch HealthStatus(s) {
	case HealthHealthy, HealthProgressing, HealthDegraded, HealthSuspended, HealthMissing:
		return HealthStatus(s)
	default:
		return HealthUnknown
	}
}

// normalizeOpPhase maps `.status.operationState.phase` to a known OpPhase. An
// absent operationState (no sync yet) or an unrecognized value is "" — Evaluate
// then relies on sync/health/revision alone, never a false failure from drift.
func normalizeOpPhase(s string) OpPhase {
	switch OpPhase(s) {
	case OpRunning, OpSucceeded, OpFailed, OpError, OpTerminating:
		return OpPhase(s)
	default:
		return ""
	}
}
