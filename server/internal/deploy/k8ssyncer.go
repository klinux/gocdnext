package deploy

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// ClusterPatcher issues an authenticated merge-patch against a registered cluster's
// k8s API at path, returning the response body. Like ClusterGetter, the credential
// never crosses this boundary. The store satisfies it (reusing the cluster registry).
type ClusterPatcher interface {
	ClusterAPIPatch(ctx context.Context, cluster string, projectID uuid.UUID, path string, body []byte) ([]byte, error)
}

// ClusterAPI is the read+write k8s-CRD transport the ArgoCD provider needs: Observe
// reads the Application, Sync patches its `.operation`. The store satisfies both.
type ClusterAPI interface {
	ClusterGetter
	ClusterPatcher
}

// k8sAppSyncer triggers an ArgoCD sync by PATCHing the Application CR's `.operation`
// (the argocd-application-controller watches for it and runs the sync). The write
// goes through the same credentialed cluster transport as the read path.
type k8sAppSyncer struct {
	patch ClusterPatcher
}

func newK8sAppSyncer(p ClusterPatcher) *k8sAppSyncer {
	return &k8sAppSyncer{patch: p}
}

func (s *k8sAppSyncer) syncApplication(ctx context.Context, target DeploymentTarget, revision string) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	body, err := syncOperationBody(revision)
	if err != nil {
		return err
	}
	_, err = s.patch.ClusterAPIPatch(ctx, target.Cluster, target.ProjectID, applicationCRDPath(target), body)
	return err
}

// syncOperationBody is the merge-patch that sets an Application's `.operation` to a
// sync. An empty revision syncs to the Application's own targetRevision; a pinned
// revision syncs exactly that SHA (what the watch correlates against). initiatedBy
// records gocdnext as the actor for the ArgoCD audit trail.
func syncOperationBody(revision string) ([]byte, error) {
	sync := map[string]any{}
	if revision != "" {
		sync["revision"] = revision
	}
	return json.Marshal(map[string]any{
		"operation": map[string]any{
			"initiatedBy": map[string]any{"username": "gocdnext"},
			"sync":        sync,
		},
	})
}
