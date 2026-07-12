package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// appFetcher returns the raw ArgoCD Application resource JSON for a target. The
// transport lives behind this seam; tests inject fixtures.
type appFetcher interface {
	fetchApplication(ctx context.Context, target DeploymentTarget) ([]byte, error)
}

// ArgoProvider is the ArgoCD-backed provider. It observes an Application and (in
// the sync increment) triggers a sync, never reconciling desired state itself.
// It implements Observe today; Sync — and thus the full DeploymentProvider
// interface — lands with the sync increment.
type ArgoProvider struct {
	fetch appFetcher
}

// NewArgoProvider wires the provider over the store-backed k8s-CRD transport: the
// server passes its cluster registry (as a ClusterGetter) and gets a provider
// that reads Applications through the target cluster's k8s API.
func NewArgoProvider(g ClusterGetter) *ArgoProvider {
	return &ArgoProvider{fetch: newK8sAppFetcher(g)}
}

// newArgoProviderWith injects an appFetcher directly — for tests that exercise
// Observe with a fixture fetcher, no cluster involved.
func newArgoProviderWith(f appFetcher) *ArgoProvider {
	return &ArgoProvider{fetch: f}
}

// Observe fetches the target's Application and returns one convergence snapshot.
func (a *ArgoProvider) Observe(ctx context.Context, target DeploymentTarget) (DeployState, error) {
	raw, err := a.fetch.fetchApplication(ctx, target)
	if err != nil {
		return DeployState{}, fmt.Errorf("deploy: fetch application %s/%s: %w", target.Namespace, target.Application, err)
	}
	return parseApplicationStatus(raw)
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
