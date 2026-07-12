package deploy

import (
	"context"
	"encoding/json"
	"fmt"
)

// appFetcher returns the raw ArgoCD Application resource JSON for a target. The
// transport lives behind this seam — a later increment wires it to the k8s
// Application CRD reached through the cluster registry (the same credential that
// will later serve the Rollout CR). Observe only parses; tests inject fixtures.
type appFetcher interface {
	fetchApplication(ctx context.Context, target DeploymentTarget) ([]byte, error)
}

// ArgoProvider is the ArgoCD-backed DeploymentProvider. It observes and (in a
// later increment) syncs an Application, never reconciling desired state itself.
type ArgoProvider struct {
	fetch appFetcher
}

// NewArgoProvider builds the provider over a transport (fetcher).
func NewArgoProvider(fetch appFetcher) *ArgoProvider {
	return &ArgoProvider{fetch: fetch}
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
// provider reads. Extra fields (and unknown enum values) are tolerated by design
// — unrecognized statuses normalize to Unknown so a controller-version drift
// degrades to "keep watching", never a false success or a crash.
type applicationStatus struct {
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	} `json:"status"`
}

func parseApplicationStatus(raw []byte) (DeployState, error) {
	var app applicationStatus
	if err := json.Unmarshal(raw, &app); err != nil {
		return DeployState{}, fmt.Errorf("deploy: decode application status: %w", err)
	}
	return DeployState{
		Sync:        normalizeSync(app.Status.Sync.Status),
		Health:      normalizeHealth(app.Status.Health.Status),
		ObservedRev: app.Status.Sync.Revision,
	}, nil
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
