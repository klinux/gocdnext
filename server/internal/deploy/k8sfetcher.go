package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/uuid"
)

// defaultAppNamespace is where ArgoCD keeps its Application CRs by convention;
// used when a target doesn't pin one.
const defaultAppNamespace = "argocd"

// ClusterGetter performs an authenticated GET against a registered cluster's k8s
// API at path, returning the response body. The credential stays inside the
// implementation (the store, reusing the cluster registry) — mirroring
// ProbeCluster, the decrypted token never crosses this boundary. Exported so the
// server can wire the store-backed transport at startup; injected so the fetcher
// is testable without a cluster.
type ClusterGetter interface {
	ClusterAPIGet(ctx context.Context, cluster string, projectID uuid.UUID, path string) ([]byte, error)
}

// k8sAppFetcher reads an ArgoCD Application through the target cluster's k8s API
// (the k8s-CRD transport, ADR-0001). It builds the Application CRD path and
// delegates the credentialed GET to the cluster registry.
type k8sAppFetcher struct {
	get ClusterGetter
}

func newK8sAppFetcher(g ClusterGetter) *k8sAppFetcher {
	return &k8sAppFetcher{get: g}
}

func (f *k8sAppFetcher) fetchApplication(ctx context.Context, target DeploymentTarget) ([]byte, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	ns := target.Namespace
	if ns == "" {
		ns = defaultAppNamespace
	}
	// PathEscape the segments defensively: names come from the platform-registered
	// target (validated at registration too), but escaping shuts the door on a
	// path-traversal name slipping into the k8s API URL.
	path := fmt.Sprintf(
		"/apis/argoproj.io/v1alpha1/namespaces/%s/applications/%s",
		url.PathEscape(ns), url.PathEscape(target.Application),
	)
	return f.get.ClusterAPIGet(ctx, target.Cluster, target.ProjectID, path)
}

// validateTarget fail-closes on a target that would build a dangerous or useless
// request: an empty Application would hit the collection endpoint (a LIST if RBAC
// allows), and an empty cluster / nil project would slip past the registry's
// access control. This is defence in depth — the registry (Inc.5) rejects the
// same at write time.
func validateTarget(t DeploymentTarget) error {
	switch {
	case t.Application == "":
		return errors.New("deploy: target has no application name")
	case t.Cluster == "":
		return errors.New("deploy: target has no cluster")
	case t.ProjectID == uuid.Nil:
		return errors.New("deploy: target has no owning project")
	}
	return nil
}
