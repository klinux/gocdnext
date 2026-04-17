package runs_test

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

	"github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func handler(t *testing.T) (*runs.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	return runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

// seedRun creates a project+pipeline+run via real ApplyProject/CreateRun
// calls so the read path exercises the same shape the live server produces.
func seedRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url, branch := "https://github.com/org/demo", "main"
	fp := domain.GitFingerprint(url, branch)
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: matID,
		Revision: "abc", Branch: "main",
		Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run.RunID
}

func TestDetail_NotFound(t *testing.T) {
	h, _ := handler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}", h.Detail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+uuid.NewString(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestDetail_InvalidUUID(t *testing.T) {
	h, _ := handler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}", h.Detail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/not-a-uuid", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestDetail_ReturnsStagesJobsAndLogs(t *testing.T) {
	h, pool := handler(t)
	runID := seedRun(t, pool)

	// Attach a log line so the tail path is exercised.
	var jobID uuid.UUID
	_ = pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id = $1 LIMIT 1`, runID,
	).Scan(&jobID)
	s := store.New(pool)
	_ = s.InsertLogLine(context.Background(), store.LogLine{
		JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "hi",
	})

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}", h.Detail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var got store.RunDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RunSummary.ID != runID {
		t.Fatalf("id mismatch")
	}
	if len(got.Stages) == 0 {
		t.Fatalf("stages = 0")
	}
	if got.ProjectSlug != "demo" {
		t.Fatalf("project_slug = %q", got.ProjectSlug)
	}

	var sawLog bool
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if len(j.Logs) == 1 && j.Logs[0].Text == "hi" {
				sawLog = true
			}
		}
	}
	if !sawLog {
		t.Fatalf("log line not returned")
	}
}

func TestDetail_LogsDisabledWhenZero(t *testing.T) {
	h, pool := handler(t)
	runID := seedRun(t, pool)

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}", h.Detail)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"?logs=0", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got store.RunDetail
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if len(j.Logs) != 0 {
				t.Fatalf("logs populated with logs=0")
			}
		}
	}
}
