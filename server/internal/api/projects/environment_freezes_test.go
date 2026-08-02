package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func freezeRouter(t *testing.T) (http.Handler, *store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/environments", h.ListEnvironments)
	r.Put("/api/v1/projects/{slug}/environment-freezes/{name}", h.FreezeEnvironment)
	r.Delete("/api/v1/projects/{slug}/environment-freezes/{name}", h.UnfreezeEnvironment)
	return r, s, pool
}

func freezeReq(t *testing.T, router http.Handler, slug, name, body string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/projects/"+slug+"/environment-freezes/"+name, strings.NewReader(body))
	if user != nil {
		req = req.WithContext(authapi.WithUser(req.Context(), *user))
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func unfreezeReq(t *testing.T, router http.Handler, slug, name string, user *store.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/projects/"+slug+"/environment-freezes/"+name, nil)
	if user != nil {
		req = req.WithContext(authapi.WithUser(req.Context(), *user))
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func listEnvs(t *testing.T, router http.Handler, slug string, user *store.User) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+slug+"/environments", nil)
	if user != nil {
		req = req.WithContext(authapi.WithUser(req.Context(), *user))
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list environments: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Environments []map[string]any `json:"environments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Environments
}

func TestFreezeEnvironment_Handler_RoundTripAndAudit(t *testing.T) {
	router, s, pool := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-api")
	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "ops@acme.io", Name: "Ops"}

	// An environment that has NEVER been deployed to can be frozen by name —
	// its `environments` row does not exist yet, and the first deploy to it is
	// exactly what the freeze has to stop.
	rr := freezeReq(t, router, "freeze-api", "production", `{"reason":"month-end close"}`, &maintainer)
	if rr.Code != http.StatusOK {
		t.Fatalf("freeze: %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["frozen"] != true || body["changed"] != true {
		t.Fatalf("body = %v, want frozen+changed", body)
	}

	st, found, err := s.EnvironmentFrozenState(ctx, projectID, "production")
	if err != nil || !found {
		t.Fatalf("state: found=%v err=%v", found, err)
	}
	// The actor is the canonical, server-derived one — never a client string.
	if st.FrozenBy != "ops@acme.io" {
		t.Errorf("frozen_by = %q, want the authenticated email", st.FrozenBy)
	}

	// Re-freezing is a no-op that reports changed=false.
	rr = freezeReq(t, router, "freeze-api", "production", `{"reason":"other"}`, &maintainer)
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusOK || body["changed"] != false {
		t.Fatalf("re-freeze: %d %v, want 200 changed=false", rr.Code, body)
	}

	// Audit is transactional and stably keyed on (project, name).
	var targetID, actorEmail string
	if err := pool.QueryRow(ctx,
		`SELECT target_id, actor_email FROM audit_events WHERE action=$1`,
		store.AuditActionEnvironmentFreeze).Scan(&targetID, &actorEmail); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if want := projectID.String() + ":production"; targetID != want {
		t.Errorf("target_id = %q, want %q", targetID, want)
	}
	if actorEmail != "ops@acme.io" {
		t.Errorf("actor_email = %q", actorEmail)
	}

	// Unfreeze, then unfreeze again (idempotent).
	rr = unfreezeReq(t, router, "freeze-api", "production", &maintainer)
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusOK || body["changed"] != true {
		t.Fatalf("unfreeze: %d %v", rr.Code, body)
	}
	rr = unfreezeReq(t, router, "freeze-api", "production", &maintainer)
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusOK || body["changed"] != false {
		t.Fatalf("second unfreeze: %d %v, want 200 changed=false", rr.Code, body)
	}
}

func TestFreezeEnvironment_Handler_RejectsBadInput(t *testing.T) {
	router, s, _ := freezeRouter(t)
	seedProjectForEnv(t, s, "freeze-api-bad")
	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "ops@acme.io"}

	tests := []struct {
		name, env, body string
		want            int
	}{
		{"missing reason", "production", `{}`, http.StatusBadRequest},
		{"blank reason", "production", `{"reason":"   "}`, http.StatusBadRequest},
		{"oversized reason", "production", `{"reason":"` + strings.Repeat("x", 501) + `"}`, http.StatusBadRequest},
		{"invalid name", "-nope", `{"reason":"r"}`, http.StatusBadRequest},
		{"malformed body", "production", `not json`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := freezeReq(t, router, "freeze-api-bad", tt.env, tt.body, &maintainer).Code; got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}

	// An unknown project is a 404, not a freeze on nothing.
	if got := freezeReq(t, router, "no-such-project", "production", `{"reason":"r"}`, &maintainer).Code; got != http.StatusNotFound {
		t.Fatalf("unknown project: %d, want 404", got)
	}
}

func TestListEnvironments_RedactsFreezeDetailBelowMaintainer(t *testing.T) {
	router, s, _ := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-redact")
	if _, err := s.EnsureEnvironment(ctx, projectID, "production"); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	if _, err := s.FreezeEnvironment(ctx, projectID, "production",
		store.FreezeActor{ID: uuid.New(), Email: "ops@acme.io"}, "incident INC-4412"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// A viewer learns THAT production is frozen — that's operational state
	// everyone needs — but not the reason or who did it: a freeze reason
	// routinely names an incident or an audit finding.
	viewer := store.User{ID: uuid.New(), Role: store.RoleViewer}
	got := listEnvs(t, router, "freeze-redact", &viewer)
	if len(got) != 1 {
		t.Fatalf("environments = %d, want 1", len(got))
	}
	if got[0]["frozen"] != true {
		t.Errorf("viewer cannot see frozen=true: %v", got[0])
	}
	if _, ok := got[0]["frozen_at"]; !ok {
		t.Errorf("viewer cannot see frozen_at: %v", got[0])
	}
	if _, leaked := got[0]["freeze_reason"]; leaked {
		t.Errorf("freeze_reason leaked to a viewer: %v", got[0])
	}
	if _, leaked := got[0]["frozen_by"]; leaked {
		t.Errorf("frozen_by leaked to a viewer: %v", got[0])
	}

	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer}
	got = listEnvs(t, router, "freeze-redact", &maintainer)
	if got[0]["freeze_reason"] != "incident INC-4412" || got[0]["frozen_by"] != "ops@acme.io" {
		t.Errorf("maintainer is missing the detail: %v", got[0])
	}

	// Auth disabled (no user in context) keeps the sensitive default visible,
	// matching /deploy-watches.
	got = listEnvs(t, router, "freeze-redact", nil)
	if got[0]["freeze_reason"] != "incident INC-4412" {
		t.Errorf("auth-disabled should see the detail: %v", got[0])
	}
}

func TestListEnvironments_SurfacesOrphanAndDeduplicates(t *testing.T) {
	router, s, _ := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-orphan")

	// `staging` exists AND is frozen -> ONE card, not two.
	if _, err := s.EnsureEnvironment(ctx, projectID, "staging"); err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	for _, env := range []string{"staging", "production", "canary"} {
		if _, err := s.FreezeEnvironment(ctx, projectID, env, store.FreezeActor{Email: "ops@acme.io"}, "r"); err != nil {
			t.Fatalf("freeze %s: %v", env, err)
		}
	}

	got := listEnvs(t, router, "freeze-orphan", nil)
	if len(got) != 3 {
		t.Fatalf("environments = %d, want 3 (staging once, plus two freeze-only rows): %v", len(got), got)
	}
	byName := map[string]map[string]any{}
	for _, e := range got {
		byName[e["name"].(string)] = e
	}
	if byName["staging"]["has_environment_row"] != true || byName["staging"]["id"] == nil {
		t.Errorf("staging should be a real row with an id: %v", byName["staging"])
	}
	// A freeze-only row emits NO id and NO created_at — a zero uuid / zero time
	// would render as "00000000-…" and "0001-01-01" in the UI.
	for _, name := range []string{"production", "canary"} {
		row := byName[name]
		if row["has_environment_row"] != false {
			t.Errorf("%s should be freeze-only: %v", name, row)
		}
		if _, present := row["id"]; present {
			t.Errorf("%s emitted an id: %v", name, row)
		}
		if _, present := row["created_at"]; present {
			t.Errorf("%s emitted a created_at: %v", name, row)
		}
		if row["frozen_at"] == nil {
			t.Errorf("%s is missing frozen_at: %v", name, row)
		}
	}
}

func TestDeleteEnvironment_LeavesAnOrphanFreezeThatStillBlocks(t *testing.T) {
	router, s, _ := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-delete")
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	if _, err := s.FreezeEnvironment(ctx, projectID, "production",
		store.FreezeActor{Email: "ops@acme.io"}, "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// The freeze references PROJECTS, not environments, so deleting the
	// environment leaves it standing. That is intentional: a delete/recreate
	// must not be a way to launder away a change-freeze.
	if _, err := s.DeleteEnvironment(ctx, projectID, envID); err != nil {
		t.Fatalf("delete env: %v", err)
	}
	if _, found, _ := s.EnvironmentFrozenState(ctx, projectID, "production"); !found {
		t.Fatal("the freeze vanished with the environment — a delete/recreate would bypass it")
	}
	// And it is still VISIBLE, hence still liftable.
	got := listEnvs(t, router, "freeze-delete", nil)
	if len(got) != 1 || got[0]["name"] != "production" || got[0]["has_environment_row"] != false {
		t.Fatalf("orphan freeze is not listed: %v", got)
	}
	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "ops@acme.io"}
	if code := unfreezeReq(t, router, "freeze-delete", "production", &maintainer).Code; code != http.StatusOK {
		t.Fatalf("unfreeze orphan: %d", code)
	}
}

// A rollback IS a deploy, so a change-freeze must refuse it — with 409
// (conflicting state, temporary), never 403 (which would read as "you lack the
// permission" and send the operator to the wrong place).
func TestRollbackEnvironment_RefusedWhileFrozen(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/environments/{envID}/rollback", h.RollbackEnvironment)
	ctx := context.Background()

	// A genuinely rollback-able target: a finished deploy job whose successful
	// revision still points at it. Anything less would be refused for its own
	// reasons and would not exercise the freeze guard at all.
	slug := "freeze-rollback"
	url := "https://github.com/org/" + slug
	fpm := store.FingerprintFor(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "release", Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fpm, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "ship", Stage: "deploy", Image: "alpine",
				Tasks:  []domain.Task{{Script: "true"}},
				Deploy: &domain.DeploySpec{Environment: "production", Version: "1.0.0"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID := applied.ProjectID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fpm).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "r", Branch: "main", Provider: "github", Delivery: slug, TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	jobID := run.JobRuns[0].ID
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='success', finished_at=NOW() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: run.RunID, JobRunID: jobID, Version: "1.0.0",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE deployment_revisions SET status='success', finished_at=NOW() WHERE id=$1`, revID); err != nil {
		t.Fatalf("mark success: %v", err)
	}

	post := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+slug+"/environments/"+envID.String()+"/rollback",
			strings.NewReader(`{"to_revision_id":"`+revID.String()+`"}`))
		r.ServeHTTP(rr, req)
		return rr
	}

	// Frozen -> 409, and NOTHING was re-queued.
	if _, err := s.FreezeEnvironment(ctx, projectID, "production",
		store.FreezeActor{Email: "ops@acme.io"}, "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	rr := post()
	if rr.Code != http.StatusConflict {
		t.Fatalf("frozen rollback = %d %s, want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "frozen") {
		t.Errorf("body %q should say the environment is frozen", rr.Body.String())
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("job status: %v", err)
	}
	if status != "success" {
		t.Fatalf("job status = %q — the refused rollback still re-queued the job", status)
	}

	// Thawed -> the same request is accepted.
	if _, err := s.UnfreezeEnvironment(ctx, projectID, "production",
		store.FreezeActor{Email: "ops@acme.io"}); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if rr := post(); rr.Code != http.StatusAccepted {
		t.Fatalf("thawed rollback = %d %s, want 202", rr.Code, rr.Body.String())
	}
}

// The freeze endpoints are maintainer-gated by the ROUTE, not by the handler, so
// the guard is only real if the middleware is in the chain. This mounts the same
// RequireMinRole(maintainer) the server wires in main.go.
func TestFreezeEndpoints_RequireMaintainerThroughTheMiddleware(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := projects.NewHandler(s, log)
	mw := authapi.NewMiddleware(s, log, true) // enforcement mode

	r := chi.NewRouter()
	r.Group(func(p chi.Router) {
		p.Use(mw.RequireMinRole(store.RoleMaintainer))
		p.Put("/api/v1/projects/{slug}/environment-freezes/{name}", h.FreezeEnvironment)
		p.Delete("/api/v1/projects/{slug}/environment-freezes/{name}", h.UnfreezeEnvironment)
	})
	projectID := seedProjectForEnv(t, s, "freeze-rbac")

	do := func(method string, user *store.User) int {
		body := strings.NewReader(`{"reason":"close"}`)
		req := httptest.NewRequest(method,
			"/api/v1/projects/freeze-rbac/environment-freezes/production", body)
		if user != nil {
			req = req.WithContext(authapi.WithUser(req.Context(), *user))
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code
	}

	// Anonymous -> 401, viewer -> 403, on BOTH verbs.
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if got := do(method, nil); got != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", method, got)
		}
		viewer := store.User{ID: uuid.New(), Role: store.RoleViewer, Email: "v@acme.io"}
		if got := do(method, &viewer); got != http.StatusForbidden {
			t.Errorf("%s viewer = %d, want 403", method, got)
		}
	}
	// Nothing above may have changed state.
	if _, found, _ := s.EnvironmentFrozenState(context.Background(), projectID, "production"); found {
		t.Fatal("a rejected request still froze the environment")
	}

	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "m@acme.io"}
	if got := do(http.MethodPut, &maintainer); got != http.StatusOK {
		t.Fatalf("maintainer freeze = %d, want 200", got)
	}
	admin := store.User{ID: uuid.New(), Role: store.RoleAdmin, Email: "a@acme.io"}
	if got := do(http.MethodDelete, &admin); got != http.StatusOK {
		t.Fatalf("admin unfreeze = %d, want 200", got)
	}
}

// Unfreezing wakes the runs the freeze was holding with a run_queued
// notification, so held jobs resume at once instead of after a full tick.
func TestUnfreezeEnvironment_NotifiesHeldRuns(t *testing.T) {
	router, s, pool := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-notify")

	// A run stamped as held by the freeze.
	runID := seedHeldRun(t, pool, "freeze-notify", "frozen-deploy:production")
	if _, err := s.FreezeEnvironment(ctx, projectID, "production",
		store.FreezeActor{Email: "ops@acme.io"}, "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	listener, err := pgx.Connect(ctx, dbtest.DSN())
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(ctx, "LISTEN "+store.RunQueuedChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "ops@acme.io"}
	rr := unfreezeReq(t, router, "freeze-notify", "production", &maintainer)
	if rr.Code != http.StatusOK {
		t.Fatalf("unfreeze: %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["woken_runs"] != float64(1) {
		t.Fatalf("woken_runs = %v, want 1", body["woken_runs"])
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := listener.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("no run_queued notification after unfreeze: %v", err)
	}
	if n.Payload != runID.String() {
		t.Fatalf("notified run = %s, want %s", n.Payload, runID)
	}
}

// The wake is BEST-EFFORT: it keys on the `frozen-deploy:` stamp, so a run that
// was never stamped (frozen in the window between the pre-scan and the admission
// re-check) is invisible to it. That is documented and acceptable ONLY because
// the periodic drain tick recovers it — this asserts the backstop exists.
func TestUnfreezeEnvironment_UnstampedRunIsRecoveredByTheDrainTick(t *testing.T) {
	router, s, pool := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-unstamped")

	// Held, but with NO queue_reason — the case the NOTIFY cannot see.
	runID := seedHeldRun(t, pool, "freeze-unstamped", "")
	if _, err := s.FreezeEnvironment(ctx, projectID, "production",
		store.FreezeActor{Email: "ops@acme.io"}, "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "ops@acme.io"}
	rr := unfreezeReq(t, router, "freeze-unstamped", "production", &maintainer)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["woken_runs"] != float64(0) {
		t.Fatalf("woken_runs = %v, want 0 (an unstamped run is invisible to the wake)", body["woken_runs"])
	}

	// The backstop: the run is still queued, so the scheduler's periodic
	// ListQueuedRunIDs sweep picks it up on the next tick.
	queued, err := s.ListQueuedRunIDs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedRunIDs: %v", err)
	}
	for _, id := range queued {
		if id == runID {
			return
		}
	}
	t.Fatal("the unstamped held run is not in the periodic drain sweep — nothing would ever recover it")
}

// seedHeldRun creates a queued run in the project, optionally stamped with a
// queue_reason, so the unfreeze wake has something to find (or deliberately not).
func seedHeldRun(t *testing.T, pool *pgxpool.Pool, slug, queueReason string) uuid.UUID {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := store.FingerprintFor(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "release", Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "ship", Stage: "deploy", Image: "alpine",
				Tasks:  []domain.Task{{Script: "true"}},
				Deploy: &domain.DeploySpec{Environment: "production", Version: "1.0.0"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "r", Branch: "main", Provider: "github", Delivery: slug, TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if queueReason != "" {
		if _, err := pool.Exec(ctx,
			`UPDATE runs SET queue_reason=$2 WHERE id=$1`, run.RunID, queueReason); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}
	return run.RunID
}

// A padded name in the URL must still wake the held runs.
//
// The store trims, so `%20production%20` freezes and unfreezes the environment
// stored as `production` either way. The wake is what breaks: it composes the
// exact `frozen-deploy:<name>` stamp the scheduler wrote, so an untrimmed name
// matches nothing, no NOTIFY goes out, and the "held jobs resume immediately"
// promise silently degrades to "wait up to a full tick". The handler therefore
// normalises ONCE, up front, and every later use reads the normalised value.
func TestUnfreezeEnvironment_PaddedNameStillWakesHeldRuns(t *testing.T) {
	router, s, pool := freezeRouter(t)
	ctx := context.Background()
	projectID := seedProjectForEnv(t, s, "freeze-padded")

	seedHeldRun(t, pool, "freeze-padded", "frozen-deploy:production")
	// Freeze through a PADDED URL segment too — it must land as `production`.
	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer, Email: "ops@acme.io"}
	if code := freezeReq(t, router, "freeze-padded", "%20production%20",
		`{"reason":"close"}`, &maintainer).Code; code != http.StatusOK {
		t.Fatalf("padded freeze: %d", code)
	}
	if _, found, _ := s.EnvironmentFrozenState(ctx, projectID, "production"); !found {
		t.Fatal("padded freeze did not land on the trimmed name")
	}

	rr := unfreezeReq(t, router, "freeze-padded", "%20production%20", &maintainer)
	if rr.Code != http.StatusOK {
		t.Fatalf("padded unfreeze: %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	// The response echoes the NORMALISED name, not the padded input.
	if body["environment"] != "production" {
		t.Errorf("environment = %v, want the normalised name", body["environment"])
	}
	if body["woken_runs"] != float64(1) {
		t.Fatalf("woken_runs = %v, want 1 — a padded name must not silently skip the wake", body["woken_runs"])
	}
}
