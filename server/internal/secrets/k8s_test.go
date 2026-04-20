package secrets_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func seedProject(t *testing.T, pool any, slug string) uuid.UUID {
	t.Helper()
	p := pool.(interface {
		// pgxpool.Pool satisfies this via interface embedding — we
		// just need a "not *pgxpool" typed seat so the compiler is
		// happy.
	})
	_ = p
	// Create a project via store.ApplyProject — easiest way.
	return uuid.Nil
}

// seedProjectStore uses the real store.ApplyProject to insert the
// project row so GetProjectByID finds it.
func seedProjectStore(t *testing.T, s *store.Store, slug string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name:   "p",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialManual, Fingerprint: "fp-" + slug, AutoUpdate: false,
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return res.ProjectID
}

func newResolver(t *testing.T, sec *corev1.Secret) (*secrets.KubernetesResolver, *store.Store, uuid.UUID) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projID := seedProjectStore(t, s, "team-alpha")

	var objs []runtime.Object
	if sec != nil {
		objs = append(objs, sec)
	}
	fakeCli := fake.NewSimpleClientset(objs...)

	r, err := secrets.NewKubernetesResolver(secrets.KubernetesResolverConfig{
		Store:     s,
		Client:    fakeCli,
		Namespace: "gocdnext",
	})
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	return r, s, projID
}

func TestKubernetesResolver_ReturnsRequestedKeys(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gocdnext-secrets-team-alpha",
			Namespace: "gocdnext",
		},
		Data: map[string][]byte{
			"GH_TOKEN":   []byte("ghp_abc"),
			"DEPLOY_KEY": []byte("key-value"),
			"UNRELATED":  []byte("nope"),
		},
	}
	r, _, projID := newResolver(t, sec)

	got, err := r.Resolve(context.Background(), projID, []string{"GH_TOKEN", "DEPLOY_KEY"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["GH_TOKEN"] != "ghp_abc" || got["DEPLOY_KEY"] != "key-value" {
		t.Errorf("values = %+v", got)
	}
	if _, ok := got["UNRELATED"]; ok {
		t.Error("UNRELATED was not asked for but leaked")
	}
}

func TestKubernetesResolver_MissingNamesSilentlyOmitted(t *testing.T) {
	// Scheduler expects unresolved names to be absent, NOT an error.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gocdnext-secrets-team-alpha",
			Namespace: "gocdnext",
		},
		Data: map[string][]byte{
			"ONE": []byte("one"),
		},
	}
	r, _, projID := newResolver(t, sec)

	got, err := r.Resolve(context.Background(), projID, []string{"ONE", "TWO"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got["ONE"] != "one" {
		t.Errorf("values = %+v", got)
	}
}

func TestKubernetesResolver_NoSecretObjectReturnsEmpty(t *testing.T) {
	// Project has no K8s Secret yet. Not an error — scheduler diffs
	// the requested names and reports them as unset.
	r, _, projID := newResolver(t, nil)
	got, err := r.Resolve(context.Background(), projID, []string{"ANY"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("values = %+v", got)
	}
}

func TestKubernetesResolver_EmptyNamesIsNoop(t *testing.T) {
	r, _, projID := newResolver(t, nil)
	got, err := r.Resolve(context.Background(), projID, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("values = %+v", got)
	}
}

func TestKubernetesResolver_RejectsMissingNamespace(t *testing.T) {
	_, err := secrets.NewKubernetesResolver(secrets.KubernetesResolverConfig{
		Store:  &store.Store{},
		Client: fake.NewSimpleClientset(),
	})
	if err == nil {
		t.Error("expected error for missing namespace")
	}
}

func TestKubernetesResolver_CustomNameTemplate(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proj-team-alpha-creds",
			Namespace: "gocdnext",
		},
		Data: map[string][]byte{"TOKEN": []byte("v")},
	}
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projID := seedProjectStore(t, s, "team-alpha")

	r, err := secrets.NewKubernetesResolver(secrets.KubernetesResolverConfig{
		Store:        s,
		Client:       fake.NewSimpleClientset(sec),
		Namespace:    "gocdnext",
		NameTemplate: "proj-{slug}-creds",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := r.Resolve(context.Background(), projID, []string{"TOKEN"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["TOKEN"] != "v" {
		t.Errorf("custom template didn't route: %+v", got)
	}
}
