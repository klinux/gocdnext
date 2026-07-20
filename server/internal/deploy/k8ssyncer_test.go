package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakePatcher records the cluster/project/path/body of a patch and returns a canned
// result, standing in for the store-backed write transport.
type fakePatcher struct {
	gotCluster string
	gotProject uuid.UUID
	gotPath    string
	gotBody    []byte
	resp       []byte
	err        error
}

func (f *fakePatcher) ClusterAPIPatch(_ context.Context, cluster string, project uuid.UUID, path string, body []byte) ([]byte, error) {
	f.gotCluster, f.gotProject, f.gotPath, f.gotBody = cluster, project, path, body
	return f.resp, f.err
}

func TestK8sAppSyncer_BuildsOperationPatch(t *testing.T) {
	proj := uuid.New()
	p := &fakePatcher{resp: []byte(`{}`)}
	s := newK8sAppSyncer(p)

	target := DeploymentTarget{ProjectID: proj, Cluster: "prod", Application: "checkout", Namespace: "argocd", SyncMode: SyncModeTrigger}
	if err := s.syncApplication(context.Background(), target, "abc123", []string{"CreateNamespace=true"}); err != nil {
		t.Fatalf("syncApplication: %v", err)
	}
	if p.gotCluster != "prod" || p.gotProject != proj {
		t.Errorf("delegated cluster/project = %q/%v", p.gotCluster, p.gotProject)
	}
	if want := "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/checkout"; p.gotPath != want {
		t.Errorf("path = %q, want %q", p.gotPath, want)
	}
	// The body must set .operation.sync.revision and record the initiator.
	var got map[string]any
	if err := json.Unmarshal(p.gotBody, &got); err != nil {
		t.Fatalf("patch body not JSON: %v (%s)", err, p.gotBody)
	}
	op, _ := got["operation"].(map[string]any)
	sync, _ := op["sync"].(map[string]any)
	if sync["revision"] != "abc123" {
		t.Errorf("operation.sync.revision = %v, want abc123 (body=%s)", sync["revision"], p.gotBody)
	}
	init, _ := op["initiatedBy"].(map[string]any)
	if init["username"] != "gocdnext" {
		t.Errorf("initiatedBy.username = %v, want gocdnext", init["username"])
	}
	// The app's sync options ride along so the manual sync honors them (e.g.
	// CreateNamespace) instead of failing "namespace not found".
	opts, _ := sync["syncOptions"].([]any)
	if len(opts) != 1 || opts[0] != "CreateNamespace=true" {
		t.Errorf("operation.sync.syncOptions = %v, want [CreateNamespace=true] (body=%s)", sync["syncOptions"], p.gotBody)
	}
}

func TestK8sAppSyncer_EmptyRevisionOmitsIt(t *testing.T) {
	p := &fakePatcher{resp: []byte(`{}`)}
	s := newK8sAppSyncer(p)
	target := DeploymentTarget{ProjectID: uuid.New(), Cluster: "c", Application: "api", SyncMode: SyncModeTrigger}
	if err := s.syncApplication(context.Background(), target, "", nil); err != nil {
		t.Fatalf("syncApplication: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(p.gotBody, &got)
	op, _ := got["operation"].(map[string]any)
	sync, _ := op["sync"].(map[string]any)
	if _, present := sync["revision"]; present {
		t.Errorf("empty revision must be omitted, got body %s", p.gotBody)
	}
	if _, present := sync["syncOptions"]; present {
		t.Errorf("no sync options → syncOptions must be omitted, got body %s", p.gotBody)
	}
}

// Fail-closed: an invalid target must never reach the patcher (no accidental write to
// the collection endpoint / an unauthorized cluster).
func TestK8sAppSyncer_ValidatesTarget(t *testing.T) {
	p := &fakePatcher{resp: []byte(`{}`)}
	s := newK8sAppSyncer(p)
	if err := s.syncApplication(context.Background(), DeploymentTarget{ProjectID: uuid.New(), Cluster: "c"}, "r", nil); err == nil {
		t.Fatal("expected a validation error for an empty application")
	}
	if p.gotPath != "" {
		t.Errorf("patcher was called (path %q) despite an invalid target", p.gotPath)
	}
}

// fakeSyncer records whether/how it was called, for the ArgoProvider.Sync tests.
type fakeSyncer struct {
	called   bool
	gotRev   string
	gotOpts  []string
	gotError error
}

func (f *fakeSyncer) syncApplication(_ context.Context, _ DeploymentTarget, revision string, syncOptions []string) error {
	f.called = true
	f.gotRev = revision
	f.gotOpts = syncOptions
	return f.gotError
}

func TestParseSyncOptions(t *testing.T) {
	if opts := parseSyncOptions([]byte(`{"spec":{"syncPolicy":{"syncOptions":["CreateNamespace=true","ServerSideApply=true"]}}}`)); len(opts) != 2 || opts[0] != "CreateNamespace=true" {
		t.Errorf("parsed = %v, want the two options", opts)
	}
	if opts := parseSyncOptions([]byte(`{"spec":{"source":{}}}`)); opts != nil {
		t.Errorf("no syncPolicy → %v, want nil", opts)
	}
	if opts := parseSyncOptions([]byte(`{not json`)); opts != nil {
		t.Errorf("malformed → %v, want nil", opts)
	}
}

func TestArgoProvider_Sync(t *testing.T) {
	t.Run("trigger mode actuates with the revision + the app's sync options", func(t *testing.T) {
		fs := &fakeSyncer{}
		// The fetched Application carries CreateNamespace; Sync must forward it.
		fetch := fakeFetcher{raw: []byte(`{"spec":{"syncPolicy":{"syncOptions":["CreateNamespace=true"]}}}`)}
		p := newArgoProviderWithSync(fetch, fs)
		target := DeploymentTarget{ProjectID: uuid.New(), Cluster: "c", Application: "a", SyncMode: SyncModeTrigger}
		if err := p.Sync(context.Background(), target, "rev9"); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if !fs.called || fs.gotRev != "rev9" {
			t.Errorf("syncer called=%v rev=%q, want called with rev9", fs.called, fs.gotRev)
		}
		if len(fs.gotOpts) != 1 || fs.gotOpts[0] != "CreateNamespace=true" {
			t.Errorf("syncer got options %v, want the app's [CreateNamespace=true]", fs.gotOpts)
		}
	})

	t.Run("observe mode is a no-op", func(t *testing.T) {
		fs := &fakeSyncer{}
		p := newArgoProviderWithSync(fakeFetcher{}, fs)
		target := DeploymentTarget{ProjectID: uuid.New(), Cluster: "c", Application: "a", SyncMode: SyncModeObserve}
		if err := p.Sync(context.Background(), target, "rev9"); err != nil {
			t.Fatalf("Sync (observe): %v", err)
		}
		if fs.called {
			t.Error("observe mode must not actuate a sync")
		}
	})

	t.Run("wraps a syncer error", func(t *testing.T) {
		sentinel := errors.New("patch rejected")
		p := newArgoProviderWithSync(fakeFetcher{}, &fakeSyncer{gotError: sentinel})
		target := DeploymentTarget{ProjectID: uuid.New(), Cluster: "c", Application: "a", SyncMode: SyncModeTrigger}
		if err := p.Sync(context.Background(), target, "r"); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
		}
	})
}
