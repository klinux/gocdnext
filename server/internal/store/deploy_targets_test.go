package store_test

import (
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestDeployTargets_UpsertResolveCount(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	projectID := seedProject(t, s, "deploy-tgt")

	// A cluster must exist for the deploy_targets.cluster FK.
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}

	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "trigger", CreatedBy: "admin@example.com",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	tgt, err := s.ResolveDeployTarget(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := store.DeployTarget{
		ProjectID: projectID, EnvironmentID: envID, Environment: "production", Provider: "argocd",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd", SyncMode: "trigger",
	}
	if tgt != want {
		t.Fatalf("resolved = %+v, want %+v", tgt, want)
	}

	if n, err := s.CountDeployTargetsForCluster(ctx, "prod-gke"); err != nil || n != 1 {
		t.Fatalf("count = %d (err %v), want 1", n, err)
	}

	// Re-registering the same environment UPDATES in place (1:1), not a second row.
	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "observe",
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	tgt2, _ := s.ResolveDeployTarget(ctx, projectID, "production")
	if tgt2.SyncMode != "observe" {
		t.Errorf("sync_mode after re-upsert = %q, want observe", tgt2.SyncMode)
	}
	if n, _ := s.CountDeployTargetsForCluster(ctx, "prod-gke"); n != 1 {
		t.Errorf("count after re-upsert = %d, want 1 (upsert, not a new row)", n)
	}

	// An environment without a target resolves to ErrDeployTargetNotFound.
	if _, err := s.ResolveDeployTarget(ctx, projectID, "staging"); !errors.Is(err, store.ErrDeployTargetNotFound) {
		t.Errorf("resolve unknown env = %v, want ErrDeployTargetNotFound", err)
	}
}

// A governing_gate (JSONB) round-trips through upsert -> resolve -> list, and clearing
// it (nil) persists as SQL NULL (observe-only again).
func TestDeployTargets_GoverningGateRoundtrip(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	projectID := seedProject(t, s, "deploy-gate")
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}

	wantGate := &store.GoverningGate{
		Approvers:      []string{"alice@example.com"},
		ApproverGroups: []string{"sre"},
		Required:       2,
		Description:    "prod canary sign-off",
	}
	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "trigger", CreatedBy: "admin@example.com",
		RolloutAware: true, GoverningGate: wantGate,
	}); err != nil {
		t.Fatalf("upsert gated: %v", err)
	}

	tgt, err := s.ResolveDeployTarget(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !store.GoverningGateEqual(tgt.GoverningGate, wantGate) {
		t.Fatalf("resolved gate = %+v, want %+v", tgt.GoverningGate, wantGate)
	}
	items, err := s.ListDeployTargets(ctx, projectID)
	if err != nil || len(items) != 1 {
		t.Fatalf("list = %d items (err %v), want 1", len(items), err)
	}
	if !store.GoverningGateEqual(items[0].GoverningGate, wantGate) {
		t.Errorf("listed gate = %+v, want %+v", items[0].GoverningGate, wantGate)
	}

	// Clearing the gate (nil) persists as NULL — the target is observe-only again.
	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "trigger",
		RolloutAware: true, GoverningGate: nil,
	}); err != nil {
		t.Fatalf("upsert ungated: %v", err)
	}
	tgt2, _ := s.ResolveDeployTarget(ctx, projectID, "production")
	if tgt2.GoverningGate != nil {
		t.Errorf("gate after clear = %+v, want nil", tgt2.GoverningGate)
	}
}

// The FK deploy_targets.cluster -> clusters(name) ON DELETE RESTRICT keeps a
// referenced cluster from being deleted out from under a target (the friendly
// store-level guard, counting via CountDeployTargetsForCluster, lands next).
func TestDeployTargets_ClusterDeleteRestrictedByFK(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	projectID := seedProject(t, s, "deploy-tgt-fk")

	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "used-cluster", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	})
	if err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}
	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "used-cluster",
		Application: "api", Namespace: "argocd", SyncMode: "trigger",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.DeleteCluster(ctx, c.ID); !errors.Is(err, store.ErrClusterInUse) {
		t.Fatalf("delete of a referenced cluster = %v, want ErrClusterInUse (FK RESTRICT mapped)", err)
	}
}
