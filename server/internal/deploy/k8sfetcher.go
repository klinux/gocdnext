package deploy

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/uuid"
)

// defaultAppNamespace is where ArgoCD keeps its Application CRs by convention;
// used when a target doesn't pin one.
const defaultAppNamespace = "argocd"

// clusterGetter performs an authenticated GET against a registered cluster's
// k8s API at path, returning the response body. The credential stays inside the
// implementation (the store, reusing the cluster registry) — mirroring
// ProbeCluster, the decrypted token never crosses this boundary. Injected so the
// fetcher is testable without a cluster.
type clusterGetter interface {
	ClusterAPIGet(ctx context.Context, cluster string, projectID uuid.UUID, path string) ([]byte, error)
}

// k8sAppFetcher reads an ArgoCD Application through the target cluster's k8s API
// (the k8s-CRD transport, ADR-0001). It builds the Application CRD path and
// delegates the credentialed GET to the cluster registry.
type k8sAppFetcher struct {
	get clusterGetter
}

func newK8sAppFetcher(g clusterGetter) *k8sAppFetcher {
	return &k8sAppFetcher{get: g}
}

func (f *k8sAppFetcher) fetchApplication(ctx context.Context, target DeploymentTarget) ([]byte, error) {
	ns := target.Namespace
	if ns == "" {
		ns = defaultAppNamespace
	}
	// PathEscape the segments defensively: names come from the platform-registered
	// target (already validated), but escaping shuts the door on a path-traversal
	// name slipping past into the k8s API URL.
	path := fmt.Sprintf(
		"/apis/argoproj.io/v1alpha1/namespaces/%s/applications/%s",
		url.PathEscape(ns), url.PathEscape(target.Application),
	)
	return f.get.ClusterAPIGet(ctx, target.Cluster, target.ProjectID, path)
}
