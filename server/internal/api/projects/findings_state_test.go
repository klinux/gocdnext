package projects_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// findingStateRouter wires just the mutation route. The actor is injected here
// (auth middleware does it in prod); RBAC itself is RequireMinRole's job, tested
// at the middleware level — this exercises the handler + store + audit.
func findingStateRouter(t *testing.T) (http.Handler, *store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authapi.WithUser(req.Context(),
				store.User{ID: uuid.New(), Email: "m@x", Role: store.RoleMaintainer})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Put("/api/v1/projects/{slug}/finding-states/{id}/state", h.SetFindingState)
	return r, s, pool
}

// seedFindingIdentity applies a project, runs one scan with a single finding,
// and returns the identity (state) id.
func seedFindingIdentity(t *testing.T, s *store.Store, pool *pgxpool.Pool, slug, fingerprint string) int64 {
	t.Helper()
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fpm := store.FingerprintFor(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "p1", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fpm, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "scan", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply %s: %v", slug, err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fpm).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "r", Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := s.ReplaceSecurityFindings(ctx, res.JobRuns[0].ID, 0, []store.FindingIn{
		{Tool: "Trivy", RuleID: "CVE-1", Severity: "high", Level: "error", Message: "m", Fingerprint: fingerprint},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM security_finding_states WHERE fingerprint=$1`, fingerprint).Scan(&id); err != nil {
		t.Fatalf("state id: %v", err)
	}
	return id
}

func putState(t *testing.T, router http.Handler, slug string, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/projects/" + slug + "/finding-states/" + strconv.FormatInt(id, 10) + "/state"
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSetFindingState_Validation(t *testing.T) {
	router, s, pool := findingStateRouter(t)
	id := seedFindingIdentity(t, s, pool, "sec-state", "fp1")

	// Invalid state → 400.
	if rr := putState(t, router, "sec-state", id, `{"state":"bogus"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid state = %d, want 400", rr.Code)
	}
	// Reason too long → 400.
	long := `{"state":"dismissed","reason":"` + strings.Repeat("x", 1001) + `"}`
	if rr := putState(t, router, "sec-state", id, long); rr.Code != http.StatusBadRequest {
		t.Fatalf("long reason = %d, want 400", rr.Code)
	}
	// Invalid id → 400.
	if rr := putState(t, router, "sec-state", 0, `{"state":"dismissed"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid id = %d, want 400", rr.Code)
	}
	// Unknown project → 404.
	if rr := putState(t, router, "nope", id, `{"state":"dismissed"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown project = %d, want 404", rr.Code)
	}
	// Unknown id (valid project) → 404.
	if rr := putState(t, router, "sec-state", 999999, `{"state":"dismissed"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", rr.Code)
	}
}

func TestSetFindingState_DismissPersistsActorAndAudits(t *testing.T) {
	router, s, pool := findingStateRouter(t)
	ctx := context.Background()
	id := seedFindingIdentity(t, s, pool, "sec-dismiss", "fp1")

	rr := putState(t, router, "sec-dismiss", id, `{"state":"dismissed","reason":"known false alarm"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("dismiss = %d, want 204, body=%s", rr.Code, rr.Body.String())
	}

	var state, reason, actorEmail string
	if err := pool.QueryRow(ctx,
		`SELECT state, state_reason, state_actor_email FROM security_finding_states WHERE id=$1`, id,
	).Scan(&state, &reason, &actorEmail); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "dismissed" || reason != "known false alarm" || actorEmail != "m@x" {
		t.Fatalf("state row = (%q,%q,%q)", state, reason, actorEmail)
	}

	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE action='security_finding.state' AND target_id=$1`, strconv.FormatInt(id, 10),
	).Scan(&audits); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if audits != 1 {
		t.Fatalf("audit events = %d, want 1", audits)
	}
}

func TestSetFindingState_CrossProjectIs404(t *testing.T) {
	router, s, pool := findingStateRouter(t)
	idA := seedFindingIdentity(t, s, pool, "sec-a", "fpa")
	seedFindingIdentity(t, s, pool, "sec-b", "fpb")

	// Project B can't mutate project A's identity.
	if rr := putState(t, router, "sec-b", idA, `{"state":"dismissed"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-project = %d, want 404", rr.Code)
	}
	// And A's identity is untouched.
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM security_finding_states WHERE id=$1`, idA).Scan(&state); err != nil {
		t.Fatalf("read: %v", err)
	}
	if state != "open" {
		t.Fatalf("cross-project mutation leaked: state=%q", state)
	}
}
