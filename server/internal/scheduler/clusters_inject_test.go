package scheduler_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestBuildAssignment_InjectsClusterKubeconfig is the security splice
// for the cluster registry: a resolved kubeconfig lands in env under
// PLUGIN_KUBECONFIG (the input kubectl/helm already consume) AND,
// verbatim, in LogMasks — it's a bearer credential and must never leak.
// An empty kubeconfig (no cluster, or in_cluster) injects nothing.
func TestBuildAssignment_InjectsClusterKubeconfig(t *testing.T) {
	run := idTokenRun(t, "deploy", nil, "webhook", "")
	job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}

	const kc = "apiVersion: v1\nkind: Config\nusers:\n- user:\n    token: s3cr3t\n"
	got, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, kc)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Env["PLUGIN_KUBECONFIG"] != kc {
		t.Errorf("PLUGIN_KUBECONFIG = %q, want the resolved kubeconfig", got.Env["PLUGIN_KUBECONFIG"])
	}
	masked := false
	for _, m := range got.LogMasks {
		if m == kc {
			masked = true
		}
	}
	if !masked {
		t.Error("kubeconfig missing from LogMasks — the cluster credential would leak into log streams")
	}

	// Empty (no cluster / in_cluster) → no PLUGIN_KUBECONFIG injected,
	// so a raw script or in-cluster SA path isn't shadowed.
	got2, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("BuildAssignment (empty): %v", err)
	}
	if _, ok := got2.Env["PLUGIN_KUBECONFIG"]; ok {
		t.Error("PLUGIN_KUBECONFIG injected for an empty cluster — should be absent")
	}
}
