package deploysvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func kindOf(t *testing.T, err error) FaultKind {
	t.Helper()
	var f *Fault
	if !errors.As(err, &f) {
		t.Fatalf("error %v is not a *Fault", err)
	}
	return f.Kind
}

// classifyValidateErr maps each typed fetch/validation failure to an HTTP-mappable
// kind — by TYPE, never by message (the review's "no string-match" requirement).
func TestClassifyValidateErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FaultKind
	}{
		{"multi-source", fmt.Errorf("x: %w", deploy.ErrMultiSource), FaultUnprocessable},
		{"unauthorized", fmt.Errorf("x: %w", store.ErrClusterNotAuthorized), FaultForbidden},
		{"cluster not found", fmt.Errorf("x: %w", store.ErrClusterNotFound), FaultNotFound},
		{"application 404", &store.ClusterAPIStatusError{Status: http.StatusNotFound}, FaultNotFound},
		{"application 401", &store.ClusterAPIStatusError{Status: http.StatusUnauthorized}, FaultForbidden},
		{"application 403", &store.ClusterAPIStatusError{Status: http.StatusForbidden}, FaultForbidden},
		{"application 500", &store.ClusterAPIStatusError{Status: http.StatusInternalServerError}, FaultUnprocessable},
		{"unreachable / unknown", errors.New("dial tcp: i/o timeout"), FaultUnprocessable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyValidateErr(tt.err).Kind; got != tt.want {
				t.Errorf("classifyValidateErr(%v).Kind = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

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
	p := &fakeProvider{err: fmt.Errorf("app: %w", deploy.ErrMultiSource)}
	reg := &fakeRegistry{envID: uuid.New()}
	r, log := newRegistrar(p, reg)

	err := r.Register(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error from ValidateSingleSource")
	}
	if kindOf(t, err) != FaultUnprocessable {
		t.Errorf("multi-source fault kind = %v, want Unprocessable", kindOf(t, err))
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
	err := r.Register(context.Background(), in)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if kindOf(t, err) != FaultInvalid {
		t.Errorf("validation fault kind = %v, want Invalid", kindOf(t, err))
	}
	if len(*log) != 0 {
		t.Fatalf("call log = %v, want empty (validation fails before the fetch)", *log)
	}
}

func TestRegister_EnsureEnvironmentError_SkipsUpsert(t *testing.T) {
	p := &fakeProvider{}
	reg := &fakeRegistry{ensureErr: errors.New("db down")}
	r, log := newRegistrar(p, reg)

	err := r.Register(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error from EnsureEnvironment")
	}
	if kindOf(t, err) != FaultInternal {
		t.Errorf("ensure-error fault kind = %v, want Internal", kindOf(t, err))
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
