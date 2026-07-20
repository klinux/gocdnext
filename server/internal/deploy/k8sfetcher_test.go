package deploy

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeGetter records the cluster/project/path it was asked for and returns a
// canned body/error, standing in for the store-backed k8s transport.
type fakeGetter struct {
	gotCluster string
	gotProject uuid.UUID
	gotPath    string
	body       []byte
	err        error
}

func (f *fakeGetter) ClusterAPIGet(_ context.Context, cluster string, project uuid.UUID, path string) ([]byte, error) {
	f.gotCluster, f.gotProject, f.gotPath = cluster, project, path
	return f.body, f.err
}

func TestK8sAppFetcher_BuildsCRDPathAndDelegates(t *testing.T) {
	proj := uuid.New()
	g := &fakeGetter{body: []byte(`{"status":{"sync":{"status":"Synced"}}}`)}
	f := newK8sAppFetcher(g)

	target := DeploymentTarget{
		ProjectID:   proj,
		Cluster:     "prod-cluster",
		Application: "checkout",
		Namespace:   "argocd",
	}
	raw, err := f.fetchApplication(context.Background(), target)
	if err != nil {
		t.Fatalf("fetchApplication: %v", err)
	}
	if string(raw) != string(g.body) {
		t.Errorf("body = %q, want passthrough %q", raw, g.body)
	}
	if g.gotCluster != "prod-cluster" || g.gotProject != proj {
		t.Errorf("delegated cluster/project = %q/%v, want prod-cluster/%v", g.gotCluster, g.gotProject, proj)
	}
	// The exact Application CRD path — a wrong path is a silent 404 in prod.
	wantPath := "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/checkout"
	if g.gotPath != wantPath {
		t.Errorf("path = %q, want %q", g.gotPath, wantPath)
	}
}

func TestK8sAppFetcher_DefaultsNamespace(t *testing.T) {
	g := &fakeGetter{body: []byte(`{}`)}
	f := newK8sAppFetcher(g)
	target := DeploymentTarget{ProjectID: uuid.New(), Cluster: "c", Application: "api"} // no Namespace → defaults
	if _, err := f.fetchApplication(context.Background(), target); err != nil {
		t.Fatalf("fetchApplication: %v", err)
	}
	want := "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/api"
	if g.gotPath != want {
		t.Errorf("path with empty namespace = %q, want %q (defaulted to argocd)", g.gotPath, want)
	}
}

func TestK8sAppFetcher_ErrorPassthrough(t *testing.T) {
	sentinel := errors.New("cluster unreachable")
	f := newK8sAppFetcher(&fakeGetter{err: sentinel})
	if _, err := f.fetchApplication(context.Background(), DeploymentTarget{ProjectID: uuid.New(), Cluster: "c", Application: "api"}); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
}

// Fail-closed on a target that would build a dangerous request — an empty
// application name hits the collection endpoint (a LIST), and an empty cluster /
// nil project would slip past cluster access control. The getter must never be
// called for these.
func TestK8sAppFetcher_ValidatesTarget(t *testing.T) {
	tests := []struct {
		name   string
		target DeploymentTarget
	}{
		{"empty application", DeploymentTarget{ProjectID: uuid.New(), Cluster: "c"}},
		{"empty cluster", DeploymentTarget{ProjectID: uuid.New(), Application: "api"}},
		{"nil project", DeploymentTarget{Cluster: "c", Application: "api"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGetter{body: []byte(`{}`)}
			if _, err := newK8sAppFetcher(g).fetchApplication(context.Background(), tt.target); err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if g.gotPath != "" {
				t.Errorf("getter was called (path %q) despite an invalid target — must fail closed before the GET", g.gotPath)
			}
		})
	}
}
