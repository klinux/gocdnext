package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func newClusterStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t)) // ResolveClusterForDispatch decrypts via authCipher
	return s, context.Background()
}

const sampleKubeconfig = "apiVersion: v1\nkind: Config\nclusters: []\n"

func TestClusters_Kubeconfig_RoundTripAndPreserve(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)

	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", Description: "prod", AuthType: store.ClusterAuthKubeconfig,
		Credential: sampleKubeconfig, CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Fatal("expected generated id")
	}

	// Cipher round-trip: dispatch resolver decrypts back to the exact
	// kubeconfig. (Get does NOT expose the credential — write-only.)
	kc, inCluster, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "prod-gke")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if inCluster {
		t.Fatal("kubeconfig cluster must not be in_cluster")
	}
	if kc != sampleKubeconfig {
		t.Fatalf("kubeconfig round-trip mismatch: %q", kc)
	}

	// Update with the preserve sentinel + a metadata change keeps the
	// sealed credential.
	if err := s.UpdateCluster(ctx, cipher, c.ID, store.ClusterInput{
		Name: "prod-gke", Description: "prod (renamed desc)", AuthType: store.ClusterAuthKubeconfig,
		Credential: store.SecretPreserveSentinel,
	}); err != nil {
		t.Fatalf("update preserve: %v", err)
	}
	kc2, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "prod-gke")
	if err != nil {
		t.Fatalf("resolve after preserve: %v", err)
	}
	if kc2 != sampleKubeconfig {
		t.Fatalf("preserve-sentinel lost the credential: %q", kc2)
	}

	if err := s.DeleteCluster(ctx, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetCluster(ctx, c.ID); !errors.Is(err, store.ErrClusterNotFound) {
		t.Fatalf("get after delete = %v, want ErrClusterNotFound", err)
	}
}

func TestClusters_Token_SynthesizesKubeconfig(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	_, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "tok", AuthType: store.ClusterAuthToken,
		APIServer: "https://k8s.example.com:6443", CACert: []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"),
		Credential: "sa-bearer-token-xyz",
	})
	if err != nil {
		t.Fatalf("insert token: %v", err)
	}
	kc, inCluster, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "tok")
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if inCluster {
		t.Fatal("token cluster must not be in_cluster")
	}
	// Synthesized kubeconfig must carry the token + server (and embed
	// the CA, not skip TLS).
	if !strings.Contains(kc, "sa-bearer-token-xyz") || !strings.Contains(kc, "https://k8s.example.com:6443") {
		t.Fatalf("synthesized kubeconfig missing token/server:\n%s", kc)
	}
	if !strings.Contains(kc, "certificate-authority-data") {
		t.Fatalf("expected CA embedded, got:\n%s", kc)
	}
}

func TestClusters_InCluster_NoCredential(t *testing.T) {
	s, ctx := newClusterStore(t)
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "agent-local", AuthType: store.ClusterAuthInCluster,
	}); err != nil {
		t.Fatalf("insert in_cluster: %v", err)
	}
	kc, inCluster, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "agent-local")
	if err != nil {
		t.Fatalf("resolve in_cluster: %v", err)
	}
	if !inCluster || kc != "" {
		t.Fatalf("in_cluster resolve = (%q, %v), want (\"\", true)", kc, inCluster)
	}
	// in_cluster must reject a credential.
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "bad", AuthType: store.ClusterAuthInCluster, Credential: "x",
	}); err == nil {
		t.Fatal("expected error: in_cluster takes no credential")
	}
}

func TestClusters_AllowedProjects_Scoping(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	projA, projB := uuid.New(), uuid.New()
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "scoped", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
		AllowedProjects: []string{projA.String()},
	}); err != nil {
		t.Fatalf("insert scoped: %v", err)
	}
	if _, _, err := s.ResolveClusterForDispatch(ctx, projA, "scoped"); err != nil {
		t.Fatalf("authorized project rejected: %v", err)
	}
	if _, _, err := s.ResolveClusterForDispatch(ctx, projB, "scoped"); err == nil {
		t.Fatal("expected unauthorized project to be refused")
	}
}

func TestClusters_NameUnique_And_ResolveMissing(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "dup", AuthType: store.ClusterAuthInCluster,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "dup", AuthType: store.ClusterAuthInCluster,
	}); err == nil {
		t.Fatal("expected unique-name violation")
	}
	if _, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "ghost"); !errors.Is(err, store.ErrClusterNotFound) {
		t.Fatalf("resolve missing = %v, want ErrClusterNotFound", err)
	}
}

func TestClusters_DeleteGuard_Usage(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	// Seed a real pipeline, then inject a Cluster ref into its stored
	// definition so the jsonpath usage query has something to find.
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "cl-usage", Name: "cl-usage",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"deploy"},
			Jobs:   []domain.Job{{Name: "ship", Stage: "deploy", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pid := res.Pipelines[0].PipelineID
	if _, err := pool.Exec(ctx,
		`UPDATE pipelines SET definition = $2 WHERE id = $1`,
		pid, []byte(`{"Name":"build","Stages":["deploy"],"Jobs":[{"Name":"ship","Stage":"deploy","Cluster":"prod"}]}`),
	); err != nil {
		t.Fatalf("inject cluster ref: %v", err)
	}

	u, err := s.CountClusterUsage(ctx, "prod")
	if err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if u.Pipelines != 1 {
		t.Fatalf("usage.Pipelines = %d, want 1", u.Pipelines)
	}
	if other, _ := s.CountClusterUsage(ctx, "nope"); other.Pipelines != 0 {
		t.Fatalf("unreferenced usage.Pipelines = %d, want 0", other.Pipelines)
	}
}
