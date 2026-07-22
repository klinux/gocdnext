package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedInflightWatch sets up project "demo" + cluster + env + an in_progress revision +
// a deploy_watch, and returns the store.
func seedInflightWatch(t *testing.T) *store.Store {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	seedProjectAndCluster(t, s) // project "demo" + cluster "prod"

	detail, err := s.GetProjectDetail(ctx, "demo", 1)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	projectID := detail.Project.ID
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Attempt: 0, Version: "1.4.2", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "abc0123456789", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	return s
}

func deployWatchesReq(r http.Handler, role string) map[string]any {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/deploy-watches", nil)
	if role != "" {
		req = req.WithContext(authapi.WithUser(req.Context(), store.User{ID: uuid.New(), Role: role}))
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body
}

// The observed AnalysisRun (PR3) is surfaced viewer-readable in the DTO JSON.
func TestListDeployWatches_AnalysisSurfaced(t *testing.T) {
	s := seedInflightWatch(t)
	ctx := context.Background()
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (n=%d)", err, len(claimed))
	}
	if _, err := s.StampRolloutObservation(ctx, claimed[0].DeploymentRevisionID, claimed[0].ClaimID,
		store.RolloutObservationInput{
			Observed: true, Phase: "Paused", PauseReason: "AnalysisRunInconclusive",
			AnalysisKind: "step", AnalysisName: "demo-2", AnalysisPhase: "Inconclusive",
			AnalysisMessage: "success-rate 0.91 < 0.95",
		}); err != nil {
		t.Fatalf("stamp analysis: %v", err)
	}

	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/deploy-watches", h.ListDeployWatches)

	v := deployWatchesReq(r, store.RoleViewer)
	vw, _ := v["deploy_watches"].([]any)
	w0, _ := vw[0].(map[string]any)
	if w0["rollout_analysis_kind"] != "step" || w0["rollout_analysis_phase"] != "Inconclusive" ||
		w0["rollout_analysis_name"] != "demo-2" || w0["rollout_analysis_message"] != "success-rate 0.91 < 0.95" {
		t.Fatalf("viewer DTO missing/incorrect analysis fields: %v", w0)
	}
}

func TestListDeployWatches_RoleSanitized(t *testing.T) {
	s := seedInflightWatch(t)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/deploy-watches", h.ListDeployWatches)

	// Maintainer sees the config (application/cluster/sync_mode).
	m := deployWatchesReq(r, store.RoleMaintainer)
	watches, _ := m["deploy_watches"].([]any)
	if len(watches) != 1 {
		t.Fatalf("maintainer watches = %v, want 1", watches)
	}
	mw, _ := watches[0].(map[string]any)
	if mw["environment"] != "production" || mw["version"] != "1.4.2" {
		t.Fatalf("maintainer view missing live fields: %v", mw)
	}
	if mw["application"] != "checkout" || mw["cluster"] != "prod" || mw["sync_mode"] != "trigger" {
		t.Fatalf("maintainer must see config, got: %v", mw)
	}

	// Viewer sees live state but NOT the config.
	v := deployWatchesReq(r, store.RoleViewer)
	vwatches, _ := v["deploy_watches"].([]any)
	vw, _ := vwatches[0].(map[string]any)
	if vw["environment"] != "production" || vw["version"] != "1.4.2" || vw["expected_revision"] != "abc0123456789" {
		t.Fatalf("viewer must see live state: %v", vw)
	}
	for _, k := range []string{"application", "cluster", "sync_mode"} {
		if _, present := vw[k]; present {
			t.Errorf("viewer leaked maintainer-only config field %q: %v", k, vw)
		}
	}
}

// The Rollout identity pinned at arm time is what the Environments card deep-links
// with, and it names a cluster + namespace — config-class. It must follow the same
// role rule as application/cluster/sync_mode, which the sanitisation test above does
// not reach because an unarmed watch leaves those columns NULL.
func TestListDeployWatches_PinnedRolloutIdentityIsMaintainerOnly(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	seedProjectAndCluster(t, s)
	detail, err := s.GetProjectDetail(ctx, "demo", 1)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	projectID := detail.Project.ID
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Attempt: 0, Version: "1.4.2", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "abc0123456789", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	// Pin the identity directly: the arming mechanics are fenced on a claim and covered
	// elsewhere; what is under test here is the HTTP layer's role sanitisation.
	if _, err := pool.Exec(ctx, `UPDATE deploy_watches
		SET gate_id = gen_random_uuid(), gate_required = 1, gate_paused_step = 2,
		    gate_rollout_cluster = 'prod-hub', gate_rollout_namespace = 'shop',
		    gate_rollout_name = 'shop-canary'
		WHERE deployment_revision_id = $1`, revID); err != nil {
		t.Fatalf("pin gate identity: %v", err)
	}

	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/deploy-watches", h.ListDeployWatches)

	pinned := []string{"gate_rollout_cluster", "gate_rollout_namespace", "gate_rollout_name"}
	want := map[string]string{
		"gate_rollout_cluster":   "prod-hub",
		"gate_rollout_namespace": "shop",
		"gate_rollout_name":      "shop-canary",
	}

	m := deployWatchesReq(r, store.RoleMaintainer)
	mws, _ := m["deploy_watches"].([]any)
	if len(mws) != 1 {
		t.Fatalf("maintainer watches = %v, want 1", mws)
	}
	mw, _ := mws[0].(map[string]any)
	for _, k := range pinned {
		if mw[k] != want[k] {
			t.Errorf("maintainer %s = %v, want %q — the card cannot build an exact link without it", k, mw[k], want[k])
		}
	}
	// The gate live-state itself stays viewer-readable; only the identity is gated.
	if mw["gate_id"] == nil {
		t.Error("maintainer must still see gate_id")
	}

	v := deployWatchesReq(r, store.RoleViewer)
	vws, _ := v["deploy_watches"].([]any)
	vw, _ := vws[0].(map[string]any)
	for _, k := range pinned {
		if _, present := vw[k]; present {
			t.Errorf("viewer leaked pinned rollout identity %q: %v", k, vw)
		}
	}
	if vw["gate_id"] == nil {
		t.Error("viewer must still see gate_id (the gate live-state is viewer-readable)")
	}
}

func TestListDeployWatches_RolloutSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	seedProjectAndCluster(t, s)
	detail, err := s.GetProjectDetail(ctx, "demo", 1)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	projectID := detail.Project.ID
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Attempt: 0, Version: "1.4.2", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "abc", DeadlineAt: time.Now().Add(10 * time.Minute),
		RolloutAware: true,
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	claimID := claimed[0].ClaimID

	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/deploy-watches", h.ListDeployWatches)

	// A KNOWN step (0) — must appear as rollout_current_step:0 (pointer, not omitted).
	if _, err := s.StampRolloutObservation(ctx, revID, claimID, store.RolloutObservationInput{
		Observed: true, Phase: "Paused", PauseReason: "CanaryPauseStep", CurrentStep: 0, StepKnown: true, StepCount: 4,
	}); err != nil {
		t.Fatalf("stamp known: %v", err)
	}
	vw := deployWatchesReq(r, store.RoleViewer)["deploy_watches"].([]any)[0].(map[string]any)
	if vw["rollout_aware"] != true || vw["rollout_phase"] != "Paused" || vw["rollout_pause_reason"] != "CanaryPauseStep" {
		t.Fatalf("viewer missing rollout live state: %v", vw)
	}
	if step, present := vw["rollout_current_step"]; !present || step.(float64) != 0 {
		t.Errorf("known step 0 must appear as rollout_current_step:0, got present=%v val=%v", present, step)
	}
	if _, present := vw["cluster"]; present { // config still viewer-hidden
		t.Errorf("viewer leaked cluster: %v", vw)
	}

	// An UNKNOWN step must be ABSENT (nil pointer omitted), not rendered as 0.
	if _, err := s.StampRolloutObservation(ctx, revID, claimID, store.RolloutObservationInput{
		Observed: true, Phase: "Progressing", StepCount: 4, // StepKnown false
	}); err != nil {
		t.Fatalf("stamp unknown: %v", err)
	}
	vw2 := deployWatchesReq(r, store.RoleViewer)["deploy_watches"].([]any)[0].(map[string]any)
	if _, present := vw2["rollout_current_step"]; present {
		t.Errorf("unknown step must be absent, got: %v", vw2["rollout_current_step"])
	}
}
