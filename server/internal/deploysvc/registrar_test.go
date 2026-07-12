package deploysvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

type fakeProvider struct {
	err       error
	gotTarget deploy.DeploymentTarget
	log       *[]string
}

func (f *fakeProvider) ValidateSingleSource(_ context.Context, t deploy.DeploymentTarget) error {
	*f.log = append(*f.log, "validate-source")
	f.gotTarget = t
	return f.err
}

type fakeRegistry struct {
	envID     uuid.UUID
	ensureErr error
	upsertErr error
	gotUpsert store.DeployTargetInput
	log       *[]string
}

func (f *fakeRegistry) EnsureEnvironment(_ context.Context, _ uuid.UUID, _ string) (uuid.UUID, error) {
	*f.log = append(*f.log, "ensure-env")
	return f.envID, f.ensureErr
}

func (f *fakeRegistry) UpsertDeployTarget(_ context.Context, in store.DeployTargetInput) error {
	*f.log = append(*f.log, "upsert")
	f.gotUpsert = in
	return f.upsertErr
}

func newRegistrar(p *fakeProvider, r *fakeRegistry) (*Registrar, *[]string) {
	log := &[]string{}
	p.log, r.log = log, log
	return New(p, r), log
}

func validInput() RegisterInput {
	return RegisterInput{
		ProjectID: uuid.New(), Environment: "production", Provider: "argocd",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd",
		SyncMode: "trigger", CreatedBy: "admin@example.com",
	}
}

func TestRegister_HappyPath_OrderAndFields(t *testing.T) {
	envID := uuid.New()
	p := &fakeProvider{}
	reg := &fakeRegistry{envID: envID}
	r, log := newRegistrar(p, reg)

	if err := r.Register(context.Background(), validInput()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Order is load-bearing: validate the Application, THEN ensure env, THEN upsert.
	if want := []string{"validate-source", "ensure-env", "upsert"}; !equal(*log, want) {
		t.Fatalf("call order = %v, want %v", *log, want)
	}
	if reg.gotUpsert.EnvironmentID != envID {
		t.Errorf("upsert env id = %v, want the EnsureEnvironment result %v", reg.gotUpsert.EnvironmentID, envID)
	}
}

func TestRegister_DefaultsAndTrims(t *testing.T) {
	p := &fakeProvider{}
	reg := &fakeRegistry{envID: uuid.New()}
	r, _ := newRegistrar(p, reg)

	in := validInput()
	in.Namespace = ""               // -> argocd
	in.Cluster = "  prod-gke  "     // trimmed
	in.Application = "  checkout  " // trimmed
	in.Environment = "  production "
	if err := r.Register(context.Background(), in); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The fetched target and the upserted row must both carry canonical values.
	if p.gotTarget.Cluster != "prod-gke" || p.gotTarget.Application != "checkout" || p.gotTarget.Namespace != "argocd" || p.gotTarget.Environment != "production" {
		t.Errorf("fetched target not normalized: %+v", p.gotTarget)
	}
	if reg.gotUpsert.Cluster != "prod-gke" || reg.gotUpsert.Application != "checkout" || reg.gotUpsert.Namespace != "argocd" {
		t.Errorf("upsert not normalized: %+v", reg.gotUpsert)
	}
}

func TestRegister_MultiSourceOrAuthzError_DoesNotTouchDB(t *testing.T) {
	p := &fakeProvider{err: errors.New("application is multi-source")}
	reg := &fakeRegistry{envID: uuid.New()}
	r, log := newRegistrar(p, reg)

	if err := r.Register(context.Background(), validInput()); err == nil {
		t.Fatal("expected an error from ValidateSingleSource")
	}
	if want := []string{"validate-source"}; !equal(*log, want) {
		t.Fatalf("call log = %v, want %v (no DB write after a failed validation)", *log, want)
	}
}

func TestRegister_FieldValidationError_BeforeAnyEffect(t *testing.T) {
	p := &fakeProvider{}
	reg := &fakeRegistry{envID: uuid.New()}
	r, log := newRegistrar(p, reg)

	in := validInput()
	in.SyncMode = "auto" // invalid enum
	if err := r.Register(context.Background(), in); err == nil {
		t.Fatal("expected a validation error")
	}
	if len(*log) != 0 {
		t.Fatalf("call log = %v, want empty (validation fails before the fetch)", *log)
	}
}

func TestRegister_EnsureEnvironmentError_SkipsUpsert(t *testing.T) {
	p := &fakeProvider{}
	reg := &fakeRegistry{ensureErr: errors.New("db down")}
	r, log := newRegistrar(p, reg)

	if err := r.Register(context.Background(), validInput()); err == nil {
		t.Fatal("expected an error from EnsureEnvironment")
	}
	if want := []string{"validate-source", "ensure-env"}; !equal(*log, want) {
		t.Fatalf("call log = %v, want %v (no upsert after a failed ensure)", *log, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
