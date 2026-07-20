package deploysvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
		// Both cluster-resolution failures collapse to 404 (oracle-safe) — the
		// missing-vs-unauthorized distinction is a cross-project existence oracle.
		{"cluster not authorized -> collapsed 404", fmt.Errorf("x: %w", store.ErrClusterNotAuthorized), FaultNotFound},
		{"cluster not found -> collapsed 404", fmt.Errorf("x: %w", store.ErrClusterNotFound), FaultNotFound},
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

// A collapsed cluster fault must carry the generic Public message (what the
// handler emits) while retaining the specific error in Err (what it logs) — and
// both sentinels must produce the SAME public message so they're indistinguishable.
func TestClassifyValidateErr_ClusterOracleCollapsed(t *testing.T) {
	for _, sentinel := range []error{store.ErrClusterNotFound, store.ErrClusterNotAuthorized} {
		f := classifyValidateErr(fmt.Errorf("resolve %q: %w", "prod-hub", sentinel))
		if f.Public != store.ClusterUnavailableMessage {
			t.Errorf("Public = %q, want the generic %q", f.Public, store.ClusterUnavailableMessage)
		}
		// The internal detail (cluster name, missing-vs-unauthorized) is preserved
		// in Err for operator logs, but must NOT be the public message.
		if f.Public == f.Error() {
			t.Errorf("public message leaks the internal error %q", f.Error())
		}
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
	envID        uuid.UUID
	ensureErr    error
	upsertErr    error
	authorizeErr error               // returned by AuthorizeClusterForProject (rollout_cluster check)
	existing     *store.DeployTarget // ResolveDeployTarget result; nil => ErrDeployTargetNotFound (a create)
	resolveErr   error               // overrides existing: ResolveDeployTarget returns this
	gotUpsert    store.DeployTargetInput
	log          *[]string
}

func (f *fakeRegistry) EnsureEnvironment(_ context.Context, _ uuid.UUID, _ string) (uuid.UUID, error) {
	*f.log = append(*f.log, "ensure-env")
	return f.envID, f.ensureErr
}

func (f *fakeRegistry) ResolveDeployTarget(_ context.Context, _ uuid.UUID, _ string) (store.DeployTarget, error) {
	*f.log = append(*f.log, "resolve-existing")
	if f.resolveErr != nil {
		return store.DeployTarget{}, f.resolveErr
	}
	if f.existing == nil {
		return store.DeployTarget{}, store.ErrDeployTargetNotFound
	}
	return *f.existing, nil
}

func (f *fakeRegistry) AuthorizeClusterForProject(_ context.Context, _ uuid.UUID, cluster string) error {
	*f.log = append(*f.log, "authorize-cluster:"+cluster)
	return f.authorizeErr
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
	// Admin by default: the baseline register is an ordinary admin operation, so the
	// separation-of-duties path (which reads the existing target) is skipped and the
	// call-order assertions below see only validate/ensure/upsert. The SoD tests set
	// CallerIsAdmin=false explicitly.
	return RegisterInput{
		ProjectID: uuid.New(), Environment: "production", Provider: "argocd",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd",
		SyncMode: "trigger", CreatedBy: "admin@example.com", CallerIsAdmin: true,
	}
}

func gate(required int, approvers ...string) *store.GoverningGate {
	return &store.GoverningGate{Required: required, Approvers: approvers}
}

func TestRegister_HappyPath_OrderAndFields(t *testing.T) {
	envID := uuid.New()
	p := &fakeProvider{}
	reg := &fakeRegistry{envID: envID}
	r, log := newRegistrar(p, reg)

	tgt, err := r.Register(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Order is load-bearing: validate the Application, THEN ensure env, THEN upsert.
	if want := []string{"validate-source", "ensure-env", "upsert"}; !equal(*log, want) {
		t.Fatalf("call order = %v, want %v", *log, want)
	}
	if reg.gotUpsert.EnvironmentID != envID {
		t.Errorf("upsert env id = %v, want the EnsureEnvironment result %v", reg.gotUpsert.EnvironmentID, envID)
	}
	// Register returns the canonical target (no read-back needed by the caller).
	if tgt.Environment != "production" || tgt.Namespace != "argocd" || tgt.Cluster != "prod-gke" || tgt.SyncMode != "trigger" {
		t.Errorf("Register returned = %+v, want the canonical target", tgt)
	}
}

func TestRegister_RolloutClusterAuthorization(t *testing.T) {
	t.Run("pinned rollout_cluster authorized after the app validate, before the write", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New()}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.RolloutAware = true
		in.RolloutCluster = "rollout-hub"
		if _, err := r.Register(context.Background(), in); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if want := []string{"validate-source", "authorize-cluster:rollout-hub", "ensure-env", "upsert"}; !equal(*log, want) {
			t.Fatalf("call order = %v, want %v", *log, want)
		}
		if reg.gotUpsert.RolloutCluster != "rollout-hub" || !reg.gotUpsert.RolloutAware {
			t.Errorf("rollout fields not upserted: %+v", reg.gotUpsert)
		}
	})

	t.Run("unpinned rollout_cluster is NOT authorized (uses the app's cluster)", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New()}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.RolloutAware = true // no RolloutCluster
		if _, err := r.Register(context.Background(), in); err != nil {
			t.Fatalf("Register: %v", err)
		}
		for _, c := range *log {
			if strings.HasPrefix(c, "authorize-cluster") {
				t.Fatalf("authorized a cluster with no rollout_cluster pinned: %v", *log)
			}
		}
	})

	t.Run("rollout_aware=false drops rollout routing (no authz, no FK reference)", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New()}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.RolloutAware = false
		in.RolloutCluster, in.RolloutNamespace, in.RolloutName = "some-cluster", "ns", "r"
		if _, err := r.Register(context.Background(), in); err != nil {
			t.Fatalf("Register: %v", err)
		}
		for _, c := range *log {
			if strings.HasPrefix(c, "authorize-cluster") {
				t.Fatalf("authorized a cluster with rollout_aware=false: %v", *log)
			}
		}
		if reg.gotUpsert.RolloutCluster != "" || reg.gotUpsert.RolloutNamespace != "" || reg.gotUpsert.RolloutName != "" {
			t.Errorf("rollout routing persisted with rollout_aware=false: %+v", reg.gotUpsert)
		}
	})

	t.Run("unauthorized rollout_cluster fails closed (collapsed) before any write", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New(), authorizeErr: fmt.Errorf("x: %w", store.ErrClusterNotAuthorized)}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.RolloutAware = true
		in.RolloutCluster = "secret-cluster"
		_, err := r.Register(context.Background(), in)
		if kindOf(t, err) != FaultNotFound { // collapsed, oracle-safe (#155)
			t.Fatalf("fault kind = %v, want FaultNotFound (collapsed)", kindOf(t, err))
		}
		for _, c := range *log {
			if c == "ensure-env" || c == "upsert" {
				t.Fatalf("write happened despite an unauthorized rollout cluster: %v", *log)
			}
		}
	})
}

// gatedTarget is an existing rollout-aware target under a gate, for the SoD tests.
func gatedTarget() *store.DeployTarget {
	return &store.DeployTarget{
		Environment: "production", Provider: "argocd", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "trigger",
		RolloutAware: true, GoverningGate: gate(2, "alice@example.com"),
	}
}

func TestRegister_GateSoD(t *testing.T) {
	// A gate with nothing to observe is rejected up front (400), regardless of role.
	t.Run("governing_gate requires rollout_aware", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New()}
		r, _ := newRegistrar(&fakeProvider{}, reg)
		in := validInput() // admin
		in.GoverningGate = gate(1)
		in.RolloutAware = false
		if kindOf(t, regErr(t, r, in)) != FaultInvalid {
			t.Fatalf("want FaultInvalid for gate-without-rollout")
		}
	})

	t.Run("required < 1 is rejected", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New()}
		r, _ := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.RolloutAware = true
		in.GoverningGate = &store.GoverningGate{Required: 0}
		if kindOf(t, regErr(t, r, in)) != FaultInvalid {
			t.Fatalf("want FaultInvalid for required<1")
		}
	})

	// Non-admin: creating a gate (nil -> set) is forbidden.
	t.Run("non-admin creating a gate is forbidden", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New()} // no existing target
		r, _ := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.CallerIsAdmin = false
		in.RolloutAware = true
		in.GoverningGate = gate(1, "bob@example.com")
		if kindOf(t, regErr(t, r, in)) != FaultForbidden {
			t.Fatalf("want FaultForbidden for non-admin creating a gate")
		}
	})

	// Non-admin: removing the gate (set -> nil) is forbidden.
	t.Run("non-admin removing a gate is forbidden", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New(), existing: gatedTarget()}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.CallerIsAdmin = false
		in.RolloutAware = true
		in.GoverningGate = nil // strip it
		if kindOf(t, regErr(t, r, in)) != FaultForbidden {
			t.Fatalf("want FaultForbidden for non-admin removing a gate")
		}
		assertNoWrite(t, *log)
	})

	// Non-admin: same gate but rerouting the rollout is forbidden.
	t.Run("non-admin rerouting a gated target is forbidden", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New(), existing: gatedTarget()}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.CallerIsAdmin = false
		in.RolloutAware = true
		in.GoverningGate = gate(2, "alice@example.com") // unchanged
		in.RolloutCluster = "other-cluster"             // reroute
		if kindOf(t, regErr(t, r, in)) != FaultForbidden {
			t.Fatalf("want FaultForbidden for non-admin reroute of a gated target")
		}
		assertNoWrite(t, *log)
	})

	// Non-admin: disabling rollout_aware on a gated target is a routing change -> forbidden.
	t.Run("non-admin disabling rollout_aware on a gated target is forbidden", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New(), existing: gatedTarget()}
		r, _ := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.CallerIsAdmin = false
		in.RolloutAware = false
		in.GoverningGate = nil // must drop the gate too (gate needs rollout_aware) — still a gate change -> 403
		if kindOf(t, regErr(t, r, in)) != FaultForbidden {
			t.Fatalf("want FaultForbidden")
		}
	})

	// Non-admin: same gate + same routing, editing a NON-gate field (application) is allowed.
	t.Run("non-admin editing a non-gate field on a gated target is allowed", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New(), existing: gatedTarget()}
		r, _ := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.CallerIsAdmin = false
		in.RolloutAware = true
		in.GoverningGate = gate(2, "alice@example.com") // round-tripped unchanged
		in.Application = "checkout-v2"                  // a non-gate, non-routing edit
		if _, err := r.Register(context.Background(), in); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if !store.GoverningGateEqual(reg.gotUpsert.GoverningGate, gate(2, "alice@example.com")) {
			t.Errorf("gate not persisted on the upsert: %+v", reg.gotUpsert.GoverningGate)
		}
	})

	// Non-admin: on an UNGATED target, changing routing stays allowed (PR1 behavior).
	t.Run("non-admin routing edit on an ungated target is allowed", func(t *testing.T) {
		ungated := &store.DeployTarget{Environment: "production", RolloutAware: false}
		reg := &fakeRegistry{envID: uuid.New(), existing: ungated}
		r, _ := newRegistrar(&fakeProvider{}, reg)
		in := validInput()
		in.CallerIsAdmin = false
		in.RolloutAware = true // turn observation on — no gate involved
		if _, err := r.Register(context.Background(), in); err != nil {
			t.Fatalf("Register: %v", err)
		}
	})

	// Admin: any gate/routing change is allowed and does NOT read the existing target.
	t.Run("admin changes a gate without a SoD read", func(t *testing.T) {
		reg := &fakeRegistry{envID: uuid.New(), existing: gatedTarget()}
		r, log := newRegistrar(&fakeProvider{}, reg)
		in := validInput() // admin
		in.RolloutAware = true
		in.GoverningGate = gate(3, "carol@example.com") // changed
		if _, err := r.Register(context.Background(), in); err != nil {
			t.Fatalf("Register: %v", err)
		}
		for _, c := range *log {
			if c == "resolve-existing" {
				t.Fatalf("admin path read the existing target (unnecessary): %v", *log)
			}
		}
	})
}

func regErr(t *testing.T, r *Registrar, in RegisterInput) error {
	t.Helper()
	_, err := r.Register(context.Background(), in)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	return err
}

func assertNoWrite(t *testing.T, log []string) {
	t.Helper()
	for _, c := range log {
		if c == "ensure-env" || c == "upsert" {
			t.Fatalf("a write happened despite a forbidden request: %v", log)
		}
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
	tgt, err := r.Register(context.Background(), in)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The fetched target, the upserted row, AND the returned target must all carry
	// canonical values.
	if p.gotTarget.Cluster != "prod-gke" || p.gotTarget.Application != "checkout" || p.gotTarget.Namespace != "argocd" || p.gotTarget.Environment != "production" {
		t.Errorf("fetched target not normalized: %+v", p.gotTarget)
	}
	if reg.gotUpsert.Cluster != "prod-gke" || reg.gotUpsert.Application != "checkout" || reg.gotUpsert.Namespace != "argocd" {
		t.Errorf("upsert not normalized: %+v", reg.gotUpsert)
	}
	if tgt.Cluster != "prod-gke" || tgt.Namespace != "argocd" || tgt.Environment != "production" {
		t.Errorf("returned target not normalized: %+v", tgt)
	}
}

func TestRegister_MultiSourceOrAuthzError_DoesNotTouchDB(t *testing.T) {
	p := &fakeProvider{err: fmt.Errorf("app: %w", deploy.ErrMultiSource)}
	reg := &fakeRegistry{envID: uuid.New()}
	r, log := newRegistrar(p, reg)

	_, err := r.Register(context.Background(), validInput())
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
	_, err := r.Register(context.Background(), in)
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

	_, err := r.Register(context.Background(), validInput())
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

// The registry must reject an environment name the pipeline parser would reject
// (same bound), so a target can't be created for an env no deploy could reference
// — and a '/' can't break the DELETE route.
func TestRegister_InvalidEnvironmentName(t *testing.T) {
	for _, bad := range []string{"bad/name", "-leading", "a b", ""} {
		p := &fakeProvider{}
		reg := &fakeRegistry{envID: uuid.New()}
		r, log := newRegistrar(p, reg)
		in := validInput()
		in.Environment = bad
		_, err := r.Register(context.Background(), in)
		if err == nil {
			t.Fatalf("env %q: expected a validation error", bad)
		}
		if kindOf(t, err) != FaultInvalid {
			t.Errorf("env %q: kind = %v, want Invalid", bad, kindOf(t, err))
		}
		if len(*log) != 0 {
			t.Errorf("env %q: call log = %v, want empty (rejected before the fetch)", bad, *log)
		}
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
