package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeRolloutActuator records Promote/Abort calls and returns canned errors — the
// handler test seam for direct actuation, no live cluster.
type fakeRolloutActuator struct {
	promoteErr error
	abortErr   error
	promoted   []deploy.DeploymentTarget
	aborted    []deploy.DeploymentTarget
}

func (f *fakeRolloutActuator) Promote(_ context.Context, t deploy.DeploymentTarget) error {
	f.promoted = append(f.promoted, t)
	return f.promoteErr
}

func (f *fakeRolloutActuator) Abort(_ context.Context, t deploy.DeploymentTarget) error {
	f.aborted = append(f.aborted, t)
	return f.abortErr
}

// fakeGateLookup returns canned armed gates (or an error) so the promote/abort guard and
// the list correlation can be exercised without seeding a full gated-deploy lifecycle.
type fakeGateLookup struct {
	gates []store.ArmedRolloutGate
	err   error
}

func (f fakeGateLookup) ListArmedRolloutGatesForCluster(_ context.Context, _ uuid.UUID, _ string) ([]store.ArmedRolloutGate, error) {
	return f.gates, f.err
}

func silentHandler(t *testing.T, s *store.Store) *projects.Handler {
	t.Helper()
	return projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newRolloutActionsRouter wires the promote/abort routes with a fake actuator + fake
// gate-lookup. A nil actuator is left UNWIRED (so the 501 path is exercised) — passing a
// typed-nil pointer would make the interface non-nil and defeat the check.
func newRolloutActionsRouter(t *testing.T, act *fakeRolloutActuator, gl fakeGateLookup) (http.Handler, *store.Store) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	h := silentHandler(t, s).WithRolloutGateLookup(gl)
	if act != nil {
		h = h.WithRolloutActuator(act)
	}
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/rollouts/{cluster}/{namespace}/{name}/promote", h.PromoteRollout)
	r.Post("/api/v1/projects/{slug}/rollouts/{cluster}/{namespace}/{name}/abort", h.AbortRollout)
	return r, s
}

const promotePath = "/api/v1/projects/demo/rollouts/prod/production/checkout/promote"
const abortPath = "/api/v1/projects/demo/rollouts/prod/production/checkout/abort"

func TestPromoteRollout_ActuatesWhenUngated(t *testing.T) {
	act := &fakeRolloutActuator{}
	r, s := newRolloutActionsRouter(t, act, fakeGateLookup{})
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodPost, promotePath, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(act.promoted) != 1 || len(act.aborted) != 0 {
		t.Fatalf("actuation calls = promote %d abort %d, want 1/0", len(act.promoted), len(act.aborted))
	}
	got := act.promoted[0]
	if got.RolloutCluster != "prod" || got.RolloutNamespace != "production" || got.RolloutName != "checkout" {
		t.Errorf("pinned target = %s/%s/%s, want prod/production/checkout", got.RolloutCluster, got.RolloutNamespace, got.RolloutName)
	}
	if got.ProjectID == uuid.Nil {
		t.Errorf("target ProjectID is nil — the guard/actuator got no project scope")
	}
}

func TestAbortRollout_ActuatesWhenUngated(t *testing.T) {
	act := &fakeRolloutActuator{}
	r, s := newRolloutActionsRouter(t, act, fakeGateLookup{})
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodPost, abortPath, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(act.aborted) != 1 || len(act.promoted) != 0 {
		t.Fatalf("actuation calls = promote %d abort %d, want 0/1", len(act.promoted), len(act.aborted))
	}
}

// A gated Rollout must NOT be directly actuated — the decision goes through the vote path.
func TestPromoteRollout_GatedIsConflict_NoActuation(t *testing.T) {
	act := &fakeRolloutActuator{}
	gl := fakeGateLookup{gates: []store.ArmedRolloutGate{
		{GateID: uuid.New(), RevisionID: uuid.New(), Required: 2, Namespace: "production", Name: "checkout", ApprovalsNow: 1},
	}}
	r, s := newRolloutActionsRouter(t, act, gl)
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodPost, promotePath, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if len(act.promoted) != 0 {
		t.Fatalf("actuator was called on a gated rollout (%d) — must be refused before actuation", len(act.promoted))
	}
}

// An armed gate on a DIFFERENT rollout doesn't block this one.
func TestPromoteRollout_GateOnOtherRolloutDoesNotBlock(t *testing.T) {
	act := &fakeRolloutActuator{}
	gl := fakeGateLookup{gates: []store.ArmedRolloutGate{
		{GateID: uuid.New(), RevisionID: uuid.New(), Required: 1, Namespace: "production", Name: "other-svc"},
	}}
	r, s := newRolloutActionsRouter(t, act, gl)
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodPost, promotePath, "")
	if rr.Code != http.StatusOK || len(act.promoted) != 1 {
		t.Fatalf("status = %d promoted = %d, want 200/1; body=%s", rr.Code, len(act.promoted), rr.Body.String())
	}
}

func TestActuateRollout_ErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		actErr error
		want   int
	}{
		{"cluster unavailable → 404", store.ErrClusterNotFound, http.StatusNotFound},
		{"cluster not authorized → 404", store.ErrClusterNotAuthorized, http.StatusNotFound},
		{"rollout not found (patch 404) → 404", &store.ClusterAPIStatusError{Status: http.StatusNotFound, Path: "/rollout"}, http.StatusNotFound},
		{"cluster 403 → 500 (generic)", &store.ClusterAPIStatusError{Status: http.StatusForbidden, Path: "/rollout"}, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act := &fakeRolloutActuator{promoteErr: tt.actErr}
			r, s := newRolloutActionsRouter(t, act, fakeGateLookup{})
			seedRolloutsProject(t, s)
			rr := doReq(r, http.MethodPost, promotePath, "")
			if rr.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tt.want, rr.Body.String())
			}
		})
	}
}

// The write guard fails CLOSED: if the armed-gate lookup errors we cannot tell whether
// the rollout is gated, so we refuse (500) rather than risk a bypass — and never actuate.
func TestPromoteRollout_GateLookupError_FailsClosed(t *testing.T) {
	act := &fakeRolloutActuator{}
	gl := fakeGateLookup{err: context.DeadlineExceeded}
	r, s := newRolloutActionsRouter(t, act, gl)
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodPost, promotePath, "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if len(act.promoted) != 0 {
		t.Fatalf("actuated despite a gate-lookup error (%d) — must fail closed", len(act.promoted))
	}
}

func TestPromoteRollout_NotConfigured_501(t *testing.T) {
	r, s := newRolloutActionsRouter(t, nil, fakeGateLookup{}) // no actuator wired
	seedRolloutsProject(t, s)
	rr := doReq(r, http.MethodPost, promotePath, "")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rr.Code, rr.Body.String())
	}
}

// --- read-side correlation: a matching armed gate rides onto the Rollout DTO ---

func newRolloutCorrelationRouter(t *testing.T, getter deploy.ClusterGetter, gl fakeGateLookup) (http.Handler, *store.Store) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	h := silentHandler(t, s).
		WithRolloutLister(deploy.NewRolloutLister(getter)).
		WithRolloutGateLookup(gl)
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/rollouts", h.ListRollouts)
	return r, s
}

// gateResp mirrors just the identity + gate of the rollouts contract for assertions.
type gateResp struct {
	Rollouts []struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Gate      *struct {
			GateID       string `json:"gate_id"`
			RevisionID   string `json:"revision_id"`
			ApprovalsNow int    `json:"approvals_now"`
			Required     int    `json:"required"`
		} `json:"gate"`
	} `json:"rollouts"`
}

func TestListRollouts_CorrelatesArmedGate(t *testing.T) {
	gateID, revID := uuid.New(), uuid.New()
	// The fixture Rollout is prod/checkout — arm a gate on exactly that identity.
	gl := fakeGateLookup{gates: []store.ArmedRolloutGate{
		{GateID: gateID, RevisionID: revID, Required: 2, Namespace: "prod", Name: "checkout", ApprovalsNow: 1},
	}}
	r, s := newRolloutCorrelationRouter(t, fakeRolloutGetter{body: []byte(rolloutListFixture)}, gl)
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/rollouts?cluster=prod&namespace=prod", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp gateResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Rollouts) != 1 {
		t.Fatalf("len(rollouts) = %d, want 1", len(resp.Rollouts))
	}
	g := resp.Rollouts[0].Gate
	if g == nil {
		t.Fatalf("rollout carries no gate; want the correlated armed gate")
	}
	if g.GateID != gateID.String() || g.RevisionID != revID.String() {
		t.Errorf("gate identity = %s/%s, want %s/%s", g.GateID, g.RevisionID, gateID, revID)
	}
	if g.ApprovalsNow != 1 || g.Required != 2 {
		t.Errorf("quorum = %d/%d, want 1/2", g.ApprovalsNow, g.Required)
	}
}

func TestListRollouts_GateOnOtherRolloutIsNull(t *testing.T) {
	gl := fakeGateLookup{gates: []store.ArmedRolloutGate{
		{GateID: uuid.New(), RevisionID: uuid.New(), Required: 1, Namespace: "prod", Name: "not-checkout"},
	}}
	r, s := newRolloutCorrelationRouter(t, fakeRolloutGetter{body: []byte(rolloutListFixture)}, gl)
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/rollouts?cluster=prod&namespace=prod", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp gateResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Rollouts) != 1 || resp.Rollouts[0].Gate != nil {
		t.Fatalf("rollout gate = %+v, want null (gate governs a different rollout)", resp.Rollouts[0].Gate)
	}
}
