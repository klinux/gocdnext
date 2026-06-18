package scheduler_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestBuildAssignment_InjectsClusterKubeconfig is the security splice
// for the cluster registry: a resolved kubeconfig lands in env under
// PLUGIN_KUBECONFIG (the input kubectl/helm already consume), and EVERY
// mask the resolver returns lands in LogMasks. The resolver hands back
// the whole blob PLUS each sensitive scalar (bearer token, client key)
// because the agent redacts line-by-line — a multiline kubeconfig as a
// whole would never match a log line, but the single-line token does.
// An empty kubeconfig (no cluster, or in_cluster) injects nothing.
func TestBuildAssignment_InjectsClusterKubeconfig(t *testing.T) {
	run := idTokenRun(t, "deploy", nil, "webhook", "")
	job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}

	const token = "super-secret-bearer-token-value"
	const kc = "apiVersion: v1\nkind: Config\nusers:\n- user:\n    token: " + token + "\n"
	masks := []string{kc, token} // what store.ResolveClusterForDispatch returns
	got, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, kc, masks)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Env["PLUGIN_KUBECONFIG"] != kc {
		t.Errorf("PLUGIN_KUBECONFIG = %q, want the resolved kubeconfig", got.Env["PLUGIN_KUBECONFIG"])
	}
	for _, want := range masks {
		found := false
		for _, m := range got.LogMasks {
			if m == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mask %q missing from LogMasks — the cluster credential would leak into log streams", want)
		}
	}

	// Empty (no cluster / in_cluster) → no PLUGIN_KUBECONFIG injected,
	// so a raw script or in-cluster SA path isn't shadowed.
	got2, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("BuildAssignment (empty): %v", err)
	}
	if _, ok := got2.Env["PLUGIN_KUBECONFIG"]; ok {
		t.Error("PLUGIN_KUBECONFIG injected for an empty cluster — should be absent")
	}
}
