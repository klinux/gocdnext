package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func vsmHandler(t *testing.T) (*projects.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	return projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

// seedFanoutProject creates one project with 3 pipelines:
//   core (git) → test stage → deploy-api (upstream: core@test)
//                             → deploy-worker (upstream: core@test)
// That's the fanout shape the VSM should draw.
func seedFanoutProject(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	coreFP := domain.GitFingerprint("https://github.com/org/core", "main")
	coreUpstreamFP := domain.UpstreamFingerprint("core", "test")

	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "fan", Name: "Fanout",
		Pipelines: []*domain.Pipeline{
			{
				Name:   "core",
				Stages: []string{"build", "test"},
				Materials: []domain.Material{{
					Type: domain.MaterialGit, Fingerprint: coreFP, AutoUpdate: true,
					Git: &domain.GitMaterial{URL: "https://github.com/org/core", Branch: "main", Events: []string{"push"}},
				}},
				Jobs: []domain.Job{
					{Name: "build", Stage: "build", Tasks: []domain.Task{{Script: "make"}}},
					{Name: "test", Stage: "test", Tasks: []domain.Task{{Script: "go test"}}},
				},
			},
			{
				Name:   "deploy-api",
				Stages: []string{"deploy"},
				Materials: []domain.Material{{
					Type: domain.MaterialUpstream, Fingerprint: coreUpstreamFP, AutoUpdate: true,
					Upstream: &domain.UpstreamMaterial{Pipeline: "core", Stage: "test", Status: "success"},
				}},
				Jobs: []domain.Job{{Name: "deploy", Stage: "deploy", Tasks: []domain.Task{{Script: "./deploy.sh"}}}},
			},
			{
				Name:   "deploy-worker",
				Stages: []string{"deploy"},
				Materials: []domain.Material{{
					Type: domain.MaterialUpstream, Fingerprint: coreUpstreamFP, AutoUpdate: true,
					Upstream: &domain.UpstreamMaterial{Pipeline: "core", Stage: "test", Status: "success"},
				}},
				Jobs: []domain.Job{{Name: "deploy", Stage: "deploy", Tasks: []domain.Task{{Script: "./deploy-worker.sh"}}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestVSM_NotFound(t *testing.T) {
	h, _ := vsmHandler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/vsm", h.VSM)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/ghost/vsm", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestVSM_ReturnsNodesAndEdges(t *testing.T) {
	h, pool := vsmHandler(t)
	seedFanoutProject(t, pool)

	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/vsm", h.VSM)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/fan/vsm", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var vsm store.VSM
	if err := json.Unmarshal(rr.Body.Bytes(), &vsm); err != nil {
		t.Fatalf("json: %v", err)
	}
	if vsm.ProjectSlug != "fan" {
		t.Errorf("slug = %q", vsm.ProjectSlug)
	}
	if len(vsm.Nodes) != 3 {
		t.Fatalf("nodes = %d", len(vsm.Nodes))
	}
	if len(vsm.Edges) != 2 {
		t.Fatalf("edges = %d", len(vsm.Edges))
	}

	// Both edges come FROM core; each goes to one downstream.
	targets := map[string]bool{}
	for _, e := range vsm.Edges {
		if e.FromPipeline != "core" {
			t.Errorf("edge from = %q, want core", e.FromPipeline)
		}
		if e.Stage != "test" {
			t.Errorf("edge stage = %q, want test", e.Stage)
		}
		targets[e.ToPipeline] = true
	}
	if !targets["deploy-api"] || !targets["deploy-worker"] {
		t.Errorf("missing downstream targets: %+v", targets)
	}

	// Core must carry the git material ref; downstreams must not
	// (they only have upstream: materials).
	var core *store.VSMNode
	for i := range vsm.Nodes {
		if vsm.Nodes[i].Name == "core" {
			core = &vsm.Nodes[i]
		}
	}
	if core == nil || len(core.GitMaterials) != 1 || core.GitMaterials[0].URL != "https://github.com/org/core" {
		t.Errorf("core git material missing/unexpected: %+v", core)
	}

	for _, n := range vsm.Nodes {
		if n.LatestRun != nil {
			t.Errorf("no runs should have happened yet, got %+v", n.LatestRun)
		}
	}
}

func TestVSM_LatestRunIsAttached(t *testing.T) {
	h, pool := vsmHandler(t)
	seedFanoutProject(t, pool)
	s := store.New(pool)

	// Trigger a run on core so the VSM has a LatestRun to show.
	ctx := context.Background()
	var coreFPID string
	_ = pool.QueryRow(ctx, `
		SELECT id FROM materials
		WHERE config->>'url' = 'https://github.com/org/core'
	`).Scan(&coreFPID)
	var coreMatID, corePipelineID [16]byte
	_ = pool.QueryRow(ctx, `SELECT id, pipeline_id FROM materials WHERE fingerprint = $1`,
		domain.GitFingerprint("https://github.com/org/core", "main"),
	).Scan(&coreMatID, &corePipelineID)

	_, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     corePipelineID,
		MaterialID:     coreMatID,
		ModificationID: 1,
		Revision:       "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:         "main",
		Provider:       "github", Delivery: "t", TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/vsm", h.VSM)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/fan/vsm", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var vsm store.VSM
	_ = json.Unmarshal(rr.Body.Bytes(), &vsm)

	var core *store.VSMNode
	for i := range vsm.Nodes {
		if vsm.Nodes[i].Name == "core" {
			core = &vsm.Nodes[i]
		}
	}
	if core == nil || core.LatestRun == nil {
		t.Fatalf("core must have a latest run; got %+v", core)
	}
	if core.LatestRun.Counter != 1 {
		t.Errorf("counter = %d", core.LatestRun.Counter)
	}
}
