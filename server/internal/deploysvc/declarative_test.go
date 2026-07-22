package deploysvc

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func declInput() DeclarativeReconcileInput {
	return DeclarativeReconcileInput{
		ProjectID:   uuid.New(),
		Environment: "production",
		Cluster:     "prod-hub",
		Application: "shop",
		Namespace:   "argocd",
		SyncMode:    "trigger",
		Actor:       "pipeline:p-1",
	}
}

func registered() *store.DeployTarget {
	return &store.DeployTarget{
		Cluster: "prod-hub", Application: "shop", Namespace: "argocd", SyncMode: "trigger",
	}
}

func TestReconcileDeclarativeTarget_NoDriftSkipsTheExpensiveWork(t *testing.T) {
	reg := &fakeRegistry{existing: registered()}
	r, log := newRegistrar(&fakeProvider{}, reg)

	res, err := r.ReconcileDeclarativeTarget(context.Background(), declInput())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != ReconcileNoChange {
		t.Fatalf("decision = %v, want NoChange", res.Decision)
	}
	// The steady state must not pay for a live Application fetch or a write on every
	// single dispatch — that is the whole point of comparing first.
	if slices.Contains(*log, "validate-source") {
		t.Error("no-drift path called ValidateSingleSource")
	}
	if slices.Contains(*log, "upsert-guarded") || slices.Contains(*log, "upsert") {
		t.Error("no-drift path wrote to the target")
	}
}

// A gated target refuses the declaration ALWAYS — including when the fields already
// match. A "no drift" short-circuit would silently ignore the declaration and quietly
// bypass the fail-loud intent.
func TestReconcileDeclarativeTarget_GatedIsTerminalEvenWithoutDrift(t *testing.T) {
	tgt := registered()
	tgt.GoverningGate = &store.GoverningGate{Required: 1}
	reg := &fakeRegistry{existing: tgt}
	r, log := newRegistrar(&fakeProvider{}, reg)

	res, err := r.ReconcileDeclarativeTarget(context.Background(), declInput())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != ReconcileTerminalFault {
		t.Fatalf("decision = %v, want TerminalFault", res.Decision)
	}
	if res.Public == "" {
		t.Error("a terminal fault must carry an actionable message")
	}
	if slices.Contains(*log, "upsert-guarded") {
		t.Error("a gated target was written to")
	}
	// It is refused before anything else is even consulted.
	if slices.Contains(*log, "authorize-declarative:prod-hub") {
		t.Error("gate check must precede the cluster authorization")
	}
}

// The authorization is credential-free and comes BEFORE the network call, so an
// unauthorized declaration never costs an ArgoCD fetch.
func TestReconcileDeclarativeTarget_AuthorizationPrecedesTheFetch(t *testing.T) {
	reg := &fakeRegistry{declarativeAuthErr: store.ErrClusterDeclarativeTargetsDisabled}
	r, log := newRegistrar(&fakeProvider{}, reg)

	res, err := r.ReconcileDeclarativeTarget(context.Background(), declInput())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != ReconcileTerminalFault {
		t.Fatalf("decision = %v, want TerminalFault", res.Decision)
	}
	// Collapsed: telling "not opted in" from "no such cluster" would leak existence.
	if res.Public != store.ClusterUnavailableMessage {
		t.Errorf("public = %q, want the collapsed cluster message", res.Public)
	}
	if slices.Contains(*log, "validate-source") {
		t.Error("an unauthorized declaration paid for an ArgoCD fetch")
	}
}

// YAML owns the base target only. A zero-value must never clear rollout config the file
// cannot express — otherwise a drift in `application` would silently disable rollout
// observation on an ungated target.
func TestReconcileDeclarativeTarget_PreservesAutoDiscoveredRolloutConfig(t *testing.T) {
	tgt := registered()
	tgt.RolloutAware = true // routing all empty => auto-discovered
	reg := &fakeRegistry{existing: tgt, envID: uuid.New()}
	r, _ := newRegistrar(&fakeProvider{}, reg)

	in := declInput()
	in.Application = "shop-v2" // drift
	res, err := r.ReconcileDeclarativeTarget(context.Background(), in)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != ReconcileChanged {
		t.Fatalf("decision = %v (%s), want Changed", res.Decision, res.Public)
	}
	if !reg.gotUpsert.RolloutAware {
		t.Error("rollout_aware was cleared by a YAML zero-value")
	}
}

// Preserving is not enough: a preserved PIN can go stale. The Application's identity is
// cluster+namespace+application, so ANY base drift would leave the watcher syncing the
// new Application while still acting on the old Rollout.
func TestReconcileDeclarativeTarget_RefusesDriftAgainstPinnedRollout(t *testing.T) {
	for _, field := range []string{"application", "namespace", "cluster"} {
		t.Run("drift in "+field, func(t *testing.T) {
			tgt := registered()
			tgt.RolloutAware = true
			tgt.RolloutName = "shop-canary" // PINNED
			reg := &fakeRegistry{existing: tgt}
			r, log := newRegistrar(&fakeProvider{}, reg)

			in := declInput()
			switch field {
			case "application":
				in.Application = "shop-v2"
			case "namespace":
				in.Namespace = "argocd-2"
			case "cluster":
				in.Cluster = "other-hub"
			}
			res, err := r.ReconcileDeclarativeTarget(context.Background(), in)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if res.Decision != ReconcileTerminalFault {
				t.Fatalf("decision = %v, want TerminalFault", res.Decision)
			}
			if slices.Contains(*log, "upsert-guarded") {
				t.Error("wrote despite a stale-pin refusal")
			}
		})
	}
}

// A lost CAS is a benign race with a concurrent edit — the job stays queued and the next
// tick decides. It must NOT be reported as a config fault.
func TestReconcileDeclarativeTarget_LostCASIsRetryNotFault(t *testing.T) {
	reg := &fakeRegistry{existing: registered(), envID: uuid.New(), guardConflict: true}
	r, _ := newRegistrar(&fakeProvider{}, reg)

	in := declInput()
	in.Application = "shop-v2"
	res, err := r.ReconcileDeclarativeTarget(context.Background(), in)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != ReconcileConflictRetry {
		t.Fatalf("decision = %v, want ConflictRetry", res.Decision)
	}
}

// The guard is built from the SAME snapshot the decision used, so a concurrent gate-add
// fails the CAS instead of being clobbered.
func TestReconcileDeclarativeTarget_GuardsAgainstTheReadSnapshot(t *testing.T) {
	tgt := registered()
	tgt.RolloutAware = true
	tgt.RolloutName = ""
	reg := &fakeRegistry{existing: tgt, envID: uuid.New()}
	r, _ := newRegistrar(&fakeProvider{}, reg)

	in := declInput()
	in.SyncMode = "observe"
	if _, err := r.ReconcileDeclarativeTarget(context.Background(), in); err != nil {
		t.Fatalf("err = %v", err)
	}
	if reg.gotGuard.RolloutAware != tgt.RolloutAware || reg.gotGuard.Gate != nil {
		t.Fatalf("guard = %+v, want it derived from the read snapshot", reg.gotGuard)
	}
}

// A create never carries a gate, and the ZERO guard is what refuses a row that raced in
// carrying one.
func TestReconcileDeclarativeTarget_CreateUsesTheZeroGuard(t *testing.T) {
	reg := &fakeRegistry{existing: nil, envID: uuid.New()}
	r, _ := newRegistrar(&fakeProvider{}, reg)

	res, err := r.ReconcileDeclarativeTarget(context.Background(), declInput())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != ReconcileChanged {
		t.Fatalf("decision = %v, want Changed", res.Decision)
	}
	if (reg.gotGuard != store.DeployTargetSoDGuard{}) {
		t.Fatalf("guard = %+v, want the zero guard on a create", reg.gotGuard)
	}
	if reg.gotUpsert.CreatedBy != "pipeline:p-1" {
		t.Errorf("created_by = %q, want the synthetic pipeline actor", reg.gotUpsert.CreatedBy)
	}
}

// The imperative classifier maps transport + 5xx to Unprocessable, which for a
// DISPATCHER would turn a transient ArgoCD outage into a permanently failed job.
func TestClassifyDeclarativeValidateErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ReconcileDecision
	}{
		{"multi-source is config", deploy.ErrMultiSource, ReconcileTerminalFault},
		{"missing application is config", &store.ClusterAPIStatusError{Status: http.StatusNotFound}, ReconcileTerminalFault},
		{"forbidden is config (RBAC won't fix itself by retrying)", &store.ClusterAPIStatusError{Status: http.StatusForbidden}, ReconcileTerminalFault},
		{"unauthorized is config", &store.ClusterAPIStatusError{Status: http.StatusUnauthorized}, ReconcileTerminalFault},
		{"an unrecognised 4xx defaults to config", &store.ClusterAPIStatusError{Status: http.StatusConflict}, ReconcileTerminalFault},
		{"rate limit is transient", &store.ClusterAPIStatusError{Status: http.StatusTooManyRequests}, ReconcileConflictRetry},
		{"timeout is transient", &store.ClusterAPIStatusError{Status: http.StatusRequestTimeout}, ReconcileConflictRetry},
		{"5xx is transient", &store.ClusterAPIStatusError{Status: http.StatusInternalServerError}, ReconcileConflictRetry},
		{"a bare transport error is transient", errors.New("dial tcp: connection refused"), ReconcileConflictRetry},
		{"cluster unavailable is config", store.ErrClusterNotFound, ReconcileTerminalFault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDeclarativeValidateErr(tt.err).Decision; got != tt.want {
				t.Fatalf("decision = %v, want %v", got, tt.want)
			}
		})
	}
}
