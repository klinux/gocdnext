package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// recordingPatcher captures the last ClusterAPIPatch call so a test can assert the
// exact path + merge-patch body the actuator issues (teeth-verify — the /status JSON
// is a contract with the argo-rollouts controller).
type recordingPatcher struct {
	calls     int
	cluster   string
	projectID uuid.UUID
	path      string
	body      []byte
	err       error
}

func (p *recordingPatcher) ClusterAPIPatch(_ context.Context, cluster string, projectID uuid.UUID, path string, body []byte) ([]byte, error) {
	p.calls++
	p.cluster, p.projectID, p.path, p.body = cluster, projectID, path, body
	return nil, p.err
}

func pinnedRolloutTarget() DeploymentTarget {
	return DeploymentTarget{
		ProjectID:        uuid.New(),
		Cluster:          "hub",
		Application:      "shop",
		RolloutAware:     true,
		RolloutCluster:   "dest",
		RolloutNamespace: "shop",
		RolloutName:      "shop-ro",
	}
}

func TestRolloutActuatorPromote(t *testing.T) {
	p := &recordingPatcher{}
	act := newK8sRolloutActuator(p)
	target := pinnedRolloutTarget()

	if err := act.promoteRollout(context.Background(), target); err != nil {
		t.Fatalf("promoteRollout: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("calls = %d, want 1", p.calls)
	}
	if p.cluster != "dest" {
		t.Errorf("cluster = %q, want dest (RolloutCluster, not the App cluster)", p.cluster)
	}
	if p.projectID != target.ProjectID {
		t.Errorf("projectID = %v, want %v", p.projectID, target.ProjectID)
	}
	if want := "/apis/argoproj.io/v1alpha1/namespaces/shop/rollouts/shop-ro/status"; p.path != want {
		t.Errorf("path = %q, want %q", p.path, want)
	}
	// Promote a step-paused canary = clear pauseConditions on the /status subresource;
	// the controller then advances one step. A null (present, not absent) is the
	// merge-patch "delete this field" — verify the KEY exists and its value is null.
	status := patchStatus(t, p.body)
	v, present := status["pauseConditions"]
	if !present {
		t.Fatalf("promote body missing status.pauseConditions: %s", p.body)
	}
	if v != nil {
		t.Errorf("status.pauseConditions = %v, want null (clear)", v)
	}
	// Must NOT set abort on a promote.
	if _, ok := status["abort"]; ok {
		t.Errorf("promote body must not touch status.abort: %s", p.body)
	}
}

func TestRolloutActuatorAbort(t *testing.T) {
	p := &recordingPatcher{}
	act := newK8sRolloutActuator(p)

	if err := act.abortRollout(context.Background(), pinnedRolloutTarget()); err != nil {
		t.Fatalf("abortRollout: %v", err)
	}
	if want := "/apis/argoproj.io/v1alpha1/namespaces/shop/rollouts/shop-ro/status"; p.path != want {
		t.Errorf("path = %q, want %q", p.path, want)
	}
	status := patchStatus(t, p.body)
	if status["abort"] != true {
		t.Errorf("status.abort = %v, want true", status["abort"])
	}
	// Abort must NOT clear pauseConditions.
	if _, ok := status["pauseConditions"]; ok {
		t.Errorf("abort body must not touch status.pauseConditions: %s", p.body)
	}
}

func TestRolloutActuatorClusterFallsBackToApp(t *testing.T) {
	p := &recordingPatcher{}
	act := newK8sRolloutActuator(p)
	target := pinnedRolloutTarget()
	target.RolloutCluster = "" // blank → act on the Application's cluster (the hub == dest in the lab)

	if err := act.promoteRollout(context.Background(), target); err != nil {
		t.Fatalf("promoteRollout: %v", err)
	}
	if p.cluster != "hub" {
		t.Errorf("cluster = %q, want hub (fallback to target.Cluster)", p.cluster)
	}
}

func TestRolloutActuatorIncompleteTarget(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*DeploymentTarget)
	}{
		{"no rollout name", func(t *DeploymentTarget) { t.RolloutName = "" }},
		{"no rollout namespace", func(t *DeploymentTarget) { t.RolloutNamespace = "" }},
		{"no cluster at all", func(t *DeploymentTarget) { t.RolloutCluster, t.Cluster = "", "" }},
		{"nil project", func(t *DeploymentTarget) { t.ProjectID = uuid.Nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &recordingPatcher{}
			act := newK8sRolloutActuator(p)
			target := pinnedRolloutTarget()
			tt.mut(&target)

			// An incomplete pin must fail BEFORE any cluster write — never issue a
			// patch to a half-built path.
			if err := act.promoteRollout(context.Background(), target); err == nil {
				t.Fatalf("promoteRollout(%s): want error, got nil", tt.name)
			}
			if p.calls != 0 {
				t.Errorf("promoteRollout(%s): issued %d patches, want 0 (fail before write)", tt.name, p.calls)
			}
		})
	}
}

func TestProviderPromoteAbortWrapErrors(t *testing.T) {
	boom := errors.New("api server said no")
	p := &recordingPatcher{err: boom}
	prov := newArgoProviderWithActuator(fakeFetcher{}, newK8sRolloutActuator(p))
	target := pinnedRolloutTarget()

	if err := prov.Promote(context.Background(), target); err == nil {
		t.Fatal("Promote: want wrapped error, got nil")
	} else if !errors.Is(err, boom) {
		t.Errorf("Promote error does not wrap the transport error: %v", err)
	}
	if err := prov.Abort(context.Background(), target); !errors.Is(err, boom) {
		t.Errorf("Abort error does not wrap the transport error: %v", err)
	}
}

func TestProviderPromoteNoActuator(t *testing.T) {
	prov := newArgoProviderWith(fakeFetcher{}) // no actuator wired
	if err := prov.Promote(context.Background(), pinnedRolloutTarget()); err == nil {
		t.Error("Promote without an actuator: want error, got nil")
	}
	if err := prov.Abort(context.Background(), pinnedRolloutTarget()); err == nil {
		t.Error("Abort without an actuator: want error, got nil")
	}
}

// patchStatus unmarshals a merge-patch body and returns its `status` object, failing
// the test if the shape is wrong.
func patchStatus(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("patch body is not JSON: %v (%s)", err, body)
	}
	status, ok := m["status"].(map[string]any)
	if !ok {
		t.Fatalf("patch body has no status object: %s", body)
	}
	return status
}
