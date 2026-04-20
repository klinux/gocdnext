package dashboard_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/dashboard"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newHandler(t *testing.T) (*dashboard.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	return dashboard.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

// seedOneRun produces a project + pipeline + single queued run so
// the dashboard queries have something to aggregate over.
func seedOneRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := store.New(pool)

	fp := domain.GitFingerprint("https://github.com/org/demo", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "demo",
		Pipelines: []*domain.Pipeline{{
			Name: "ci", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/demo", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     applied.Pipelines[0].PipelineID,
		MaterialID:     matID,
		ModificationID: 1,
		Revision:       "abc",
		Branch:         "main", Provider: "github", Delivery: "t", TriggeredBy: "system",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return res.RunID
}

func TestMetrics_EmptyDatabase(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/metrics", nil)
	rr := httptest.NewRecorder()
	h.Metrics(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got store.DashboardMetrics
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.RunsToday != 0 || got.QueuedRuns != 0 {
		t.Errorf("expected zeros, got %+v", got)
	}
}

func TestMetrics_CountsRunsToday(t *testing.T) {
	h, pool := newHandler(t)
	_ = seedOneRun(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/metrics", nil)
	rr := httptest.NewRecorder()
	h.Metrics(rr, req)
	var got store.DashboardMetrics
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.RunsToday != 1 {
		t.Errorf("runs_today = %d", got.RunsToday)
	}
	if got.QueuedRuns != 1 {
		t.Errorf("queued_runs = %d", got.QueuedRuns)
	}
}

func TestRunsGlobal_ReturnsRecentFirstWithTotal(t *testing.T) {
	h, pool := newHandler(t)
	_ = seedOneRun(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs?limit=5", nil)
	rr := httptest.NewRecorder()
	h.RunsGlobal(rr, req)

	var got struct {
		Runs   []store.GlobalRunSummary `json:"runs"`
		Total  int64                    `json:"total"`
		Limit  int32                    `json:"limit"`
		Offset int64                    `json:"offset"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Runs) != 1 || got.Total != 1 || got.Limit != 5 || got.Offset != 0 {
		t.Fatalf("envelope = %+v", got)
	}
	if got.Runs[0].ProjectSlug != "demo" || got.Runs[0].PipelineName != "ci" {
		t.Errorf("unexpected run: %+v", got.Runs[0])
	}
}

func TestRunsGlobal_StatusFilter(t *testing.T) {
	h, pool := newHandler(t)
	_ = seedOneRun(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs?status=success", nil)
	rr := httptest.NewRecorder()
	h.RunsGlobal(rr, req)

	var got struct {
		Runs  []store.GlobalRunSummary `json:"runs"`
		Total int64                    `json:"total"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Runs) != 0 || got.Total != 0 {
		t.Errorf("status=success filter on queued run: runs=%d total=%d", len(got.Runs), got.Total)
	}
}

func TestRunsGlobal_ProjectFilter(t *testing.T) {
	h, pool := newHandler(t)
	_ = seedOneRun(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs?project=other", nil)
	rr := httptest.NewRecorder()
	h.RunsGlobal(rr, req)

	var got struct {
		Runs  []store.GlobalRunSummary `json:"runs"`
		Total int64                    `json:"total"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Runs) != 0 || got.Total != 0 {
		t.Errorf("project=other should match nothing, got %+v", got)
	}
}

func TestRunsGlobal_InvalidLimit(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs?limit=0", nil)
	rr := httptest.NewRecorder()
	h.RunsGlobal(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestRunsGlobal_InvalidOffset(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs?offset=-1", nil)
	rr := httptest.NewRecorder()
	h.RunsGlobal(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestAgents_ListsSeededAgent(t *testing.T) {
	h, pool := newHandler(t)
	// Insert a raw row so we don't need the whole registration flow.
	_, err := pool.Exec(context.Background(), `
		INSERT INTO agents (name, token_hash, status, tags, capacity, last_seen_at)
		VALUES ('worker-a', 'hash', 'online', ARRAY['docker','linux']::text[], 4, NOW())
	`)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.Agents(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Agents []store.AgentSummary `json:"agents"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %d", len(got.Agents))
	}
	a := got.Agents[0]
	if a.Name != "worker-a" || a.Capacity != 4 || a.HealthState != "online" {
		t.Errorf("unexpected agent: %+v", a)
	}
}

func TestAgentDetail_ReturnsSingleAgent(t *testing.T) {
	h, pool := newHandler(t)
	var agentID uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO agents (name, token_hash, status, tags, capacity, last_seen_at)
		VALUES ('worker-b', 'hash', 'online', ARRAY['linux']::text[], 2, NOW())
		RETURNING id
	`).Scan(&agentID)
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/api/v1/agents/{id}", h.AgentDetail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID.String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Agent store.AgentSummary      `json:"agent"`
		Jobs  []store.AgentJobSummary `json:"jobs"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Agent.Name != "worker-b" || got.Agent.HealthState != "online" {
		t.Errorf("agent = %+v", got.Agent)
	}
	if len(got.Jobs) != 0 {
		t.Errorf("fresh agent should have no jobs, got %d", len(got.Jobs))
	}
}

func TestAgentDetail_NotFound(t *testing.T) {
	h, _ := newHandler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/agents/{id}", h.AgentDetail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.NewString(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestAgentDetail_InvalidID(t *testing.T) {
	h, _ := newHandler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/agents/{id}", h.AgentDetail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/not-a-uuid", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}
