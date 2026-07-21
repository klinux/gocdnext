package deploy

import (
	"context"

	"github.com/google/uuid"
)

// RolloutLister lists Argo Rollouts in a namespace through the cluster registry's
// credentialed k8s transport (ADR-0001) — the read side behind the rollouts
// dashboard. It reuses the same ClusterGetter as the provider's Observe path, so the
// decrypted credential never crosses this boundary; project access is gated by the
// registry (allowed_projects) inside ClusterAPIGet, not here.
type RolloutLister struct {
	fetch *k8sAppFetcher
}

// NewRolloutLister wires the lister over a ClusterGetter (the store satisfies it).
// Exported so the server can wire the store-backed transport at startup.
func NewRolloutLister(g ClusterGetter) *RolloutLister {
	return &RolloutLister{fetch: newK8sAppFetcher(g)}
}

// ListRollouts returns every Rollout in namespace on cluster as rich views. namespace
// is REQUIRED — a cluster-wide list is a deliberate follow-up. projectID gates cluster
// access; an unresolvable/forbidden cluster surfaces as a store cluster error that the
// caller collapses to a single not-found (no cross-project existence oracle).
func (l *RolloutLister) ListRollouts(ctx context.Context, cluster string, projectID uuid.UUID, namespace string) ([]RolloutView, error) {
	return l.fetch.listRollouts(ctx, cluster, projectID, namespace)
}
