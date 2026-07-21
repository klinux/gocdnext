package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeRolloutGetter stands in for the store-backed k8s transport: it returns a canned
// RolloutList body/error, so the handler test exercises the real list + parse path
// (deploy.NewRolloutLister) without a live cluster.
type fakeRolloutGetter struct {
	body []byte
	err  error
}

func (f fakeRolloutGetter) ClusterAPIGet(_ context.Context, _ string, _ uuid.UUID, _ string) ([]byte, error) {
	return f.body, f.err
}

func newRolloutsRouter(t *testing.T, g deploy.ClusterGetter) (http.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if g != nil {
		h = h.WithRolloutLister(deploy.NewRolloutLister(g))
	}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/rollouts", h.ListRollouts)
	return r, s
}

func seedRolloutsProject(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// A canary mid-rollout with steps, a reported weight, and a step AnalysisRun — the
// fixture the k8s API would return for GET .../rollouts.
const rolloutListFixture = `{"items":[{` +
	`"metadata":{"namespace":"prod","name":"checkout"},` +
	`"spec":{"strategy":{"canary":{"steps":[{"setWeight":25},{"pause":{}},{"setWeight":50}]}},` +
	`"template":{"spec":{"containers":[{"image":"reg.example.com/checkout:v2"}]}}},` +
	`"status":{"phase":"Paused","message":"CanaryPauseStep","currentStepIndex":1,` +
	`"stableRS":"abc","currentPodHash":"def",` +
	`"canary":{"weights":{"canary":{"weight":25}},` +
	`"currentStepAnalysisRunStatus":{"name":"checkout-analysis","status":"Running","message":"measuring"}}}` +
	`}]}`

// rolloutsResp mirrors the handler's snake_case JSON contract for assertions.
type rolloutsResp struct {
	Rollouts []struct {
		Namespace        string `json:"namespace"`
		Name             string `json:"name"`
		Strategy         string `json:"strategy"`
		Phase            string `json:"phase"`
		CurrentStepIndex int    `json:"current_step_index"`
		CurrentStepKnown bool   `json:"current_step_known"`
		CanaryWeight     int    `json:"canary_weight"`
		StableHash       string `json:"stable_hash"`
		PodHash          string `json:"pod_hash"`
		Image            string `json:"image"`
		Steps            []struct {
			Kind          string `json:"kind"`
			Weight        *int   `json:"weight"`
			PauseDuration string `json:"pause_duration"`
		} `json:"steps"`
		Analysis *struct {
			Name    string `json:"name"`
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"analysis"`
	} `json:"rollouts"`
}

func TestListRollouts_OKShape(t *testing.T) {
	r, s := newRolloutsRouter(t, fakeRolloutGetter{body: []byte(rolloutListFixture)})
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/rollouts?cluster=prod&namespace=prod", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp rolloutsResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Rollouts) != 1 {
		t.Fatalf("len(rollouts) = %d, want 1", len(resp.Rollouts))
	}
	ro := resp.Rollouts[0]
	if ro.Namespace != "prod" || ro.Name != "checkout" || ro.Strategy != "canary" {
		t.Errorf("identity/strategy wrong: %+v", ro)
	}
	if ro.Phase != "Paused" || ro.CurrentStepIndex != 1 || !ro.CurrentStepKnown {
		t.Errorf("phase/step wrong: %+v", ro)
	}
	if ro.CanaryWeight != 25 || ro.StableHash != "abc" || ro.PodHash != "def" ||
		ro.Image != "reg.example.com/checkout:v2" {
		t.Errorf("weight/hashes/image wrong: %+v", ro)
	}
	if len(ro.Steps) != 3 {
		t.Fatalf("len(steps) = %d, want 3", len(ro.Steps))
	}
	if ro.Steps[0].Kind != "setWeight" || ro.Steps[0].Weight == nil || *ro.Steps[0].Weight != 25 {
		t.Errorf("step[0] wrong: %+v", ro.Steps[0])
	}
	if ro.Steps[1].Kind != "pause" || ro.Steps[1].PauseDuration != "" || ro.Steps[1].Weight != nil {
		t.Errorf("step[1] (indefinite pause) wrong: %+v", ro.Steps[1])
	}
	if ro.Analysis == nil || ro.Analysis.Name != "checkout-analysis" ||
		ro.Analysis.Phase != "Running" || ro.Analysis.Message != "measuring" {
		t.Errorf("analysis wrong: %+v", ro.Analysis)
	}
}

func TestListRollouts_MissingQueryParams(t *testing.T) {
	r, s := newRolloutsRouter(t, fakeRolloutGetter{body: []byte(`{"items":[]}`)})
	seedRolloutsProject(t, s)

	tests := []struct {
		name string
		path string
	}{
		{"missing cluster", "/api/v1/projects/demo/rollouts?namespace=prod"},
		{"blank cluster", "/api/v1/projects/demo/rollouts?cluster=%20&namespace=prod"},
		{"missing namespace", "/api/v1/projects/demo/rollouts?cluster=prod"},
		{"blank namespace", "/api/v1/projects/demo/rollouts?cluster=prod&namespace=%20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := doReq(r, http.MethodGet, tt.path, "")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// A cluster that doesn't resolve (or the project isn't allowed) collapses to a single
// 404 + generic body — no missing-vs-unauthorized oracle, no internal detail leaked.
func TestListRollouts_ClusterUnavailable_404(t *testing.T) {
	r, s := newRolloutsRouter(t, fakeRolloutGetter{err: store.ErrClusterNotFound})
	seedRolloutsProject(t, s)

	rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/rollouts?cluster=ghost&namespace=prod", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, store.ClusterUnavailableMessage) {
		t.Errorf("want generic %q, got: %s", store.ClusterUnavailableMessage, body)
	}
}

func TestListRollouts_NotConfigured_501(t *testing.T) {
	r, s := newRolloutsRouter(t, nil) // no lister wired
	seedRolloutsProject(t, s)
	rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/rollouts?cluster=prod&namespace=prod", "")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rr.Code, rr.Body.String())
	}
}
