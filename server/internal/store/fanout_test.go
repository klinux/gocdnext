package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedFanoutProject applies a 3-pipeline project matching examples/fanout:
// build-core is the upstream, deploy-api and deploy-worker depend on its
// `test` stage. Returns ids handy for the assertions.
func seedFanoutProject(t *testing.T, pool *pgxpool.Pool) (coreID, apiID, workerID, coreMaterialID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	coreGitURL := "https://github.com/org/core"
	coreFP := domain.GitFingerprint(coreGitURL, "main")

	pipelines := []*domain.Pipeline{
		{
			Name:   "build-core",
			Stages: []string{"build", "test"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: coreFP, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: coreGitURL, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "compile", Stage: "build", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make"}}},
				{Name: "unit", Stage: "test", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make test"}}},
			},
		},
		{
			Name:   "deploy-api",
			Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type:        domain.MaterialUpstream,
				Fingerprint: domain.UpstreamFingerprint("build-core", "test"),
				AutoUpdate:  true,
				Upstream:    &domain.UpstreamMaterial{Pipeline: "build-core", Stage: "test", Status: "success"},
			}},
			Jobs: []domain.Job{
				{Name: "deploy", Stage: "deploy", Image: "local", Tasks: []domain.Task{{Script: "echo api"}}},
			},
		},
		{
			Name:   "deploy-worker",
			Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type:        domain.MaterialUpstream,
				Fingerprint: domain.UpstreamFingerprint("build-core", "test"),
				AutoUpdate:  true,
				Upstream:    &domain.UpstreamMaterial{Pipeline: "build-core", Stage: "test", Status: "success"},
			}},
			Jobs: []domain.Job{
				{Name: "deploy", Stage: "deploy", Image: "local", Tasks: []domain.Task{{Script: "echo worker"}}},
			},
		},
	}

	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "fanout", Name: "Fanout", Pipelines: pipelines,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, p := range res.Pipelines {
		switch p.Name {
		case "build-core":
			coreID = p.PipelineID
		case "deploy-api":
			apiID = p.PipelineID
		case "deploy-worker":
			workerID = p.PipelineID
		}
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, coreFP).Scan(&coreMaterialID); err != nil {
		t.Fatalf("core mat lookup: %v", err)
	}
	return
}

// completeUpstreamTestStage triggers a run on build-core and walks it to the
// point where the `test` stage is green, matching what Connect.handleJobResult
// would do in the live server.
func completeUpstreamTestStage(t *testing.T, pool *pgxpool.Pool, coreID, coreMaterialID uuid.UUID) (runID, testStageID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: coreID, MaterialID: coreMaterialID,
		Revision: "abc123abc123abc123abc123abc123abc123abc1", Branch: "main",
		Provider: "github", Delivery: "fanout-test", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("core run: %v", err)
	}
	runID = run.RunID
	for _, st := range run.StageRuns {
		if st.Name == "test" {
			testStageID = st.ID
		}
	}

	// Seed an agent row for FK + flip both jobs straight to running, then
	// complete them. The scheduler logic is skipped — we only care about the
	// state machine effect on stage_runs / runs.
	var agentID uuid.UUID
	_ = pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, 'h') RETURNING id`, "fanout-"+runID.String()[:8],
	).Scan(&agentID)

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', started_at=NOW(), agent_id=$1 WHERE run_id=$2`,
		agentID, runID,
	); err != nil {
		t.Fatalf("flip running: %v", err)
	}

	for _, j := range run.JobRuns {
		if _, _, err := s.CompleteJob(ctx, store.CompleteJobInput{
			JobRunID: j.ID, Status: "success",
		}); err != nil {
			t.Fatalf("complete %s: %v", j.Name, err)
		}
	}
	return
}

func TestFanoutFromStage_TriggersDownstreams(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, apiID, workerID, coreMat := seedFanoutProject(t, pool)
	coreRunID, testStageID := completeUpstreamTestStage(t, pool, coreID, coreMat)

	triggered, err := s.FanoutFromStage(ctx, testStageID)
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	if len(triggered) != 2 {
		t.Fatalf("triggered = %d, want 2 (api + worker)", len(triggered))
	}

	seen := map[uuid.UUID]bool{}
	for _, tr := range triggered {
		if !tr.Created {
			t.Fatalf("downstream should be freshly created: %+v", tr)
		}
		seen[tr.DownstreamPipelineID] = true
	}
	if !seen[apiID] || !seen[workerID] {
		t.Fatalf("downstreams seen = %v, want both api+worker", seen)
	}

	// Each downstream run must reference the upstream_run_id.
	for _, tr := range triggered {
		var upstreamRunID string
		if err := pool.QueryRow(ctx,
			`SELECT cause_detail->>'upstream_run_id' FROM runs WHERE id = $1`,
			tr.Run.RunID,
		).Scan(&upstreamRunID); err != nil {
			t.Fatalf("cause_detail lookup: %v", err)
		}
		if upstreamRunID != coreRunID.String() {
			t.Fatalf("downstream %s has upstream_run_id=%q, want %s",
				tr.DownstreamPipelineID, upstreamRunID, coreRunID)
		}
	}
}

func TestFanoutFromStage_IsIdempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, _, _, coreMat := seedFanoutProject(t, pool)
	_, testStageID := completeUpstreamTestStage(t, pool, coreID, coreMat)

	first, err := s.FanoutFromStage(ctx, testStageID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.FanoutFromStage(ctx, testStageID)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	for _, tr := range second {
		if tr.Created {
			t.Fatalf("second call created a new run for %s", tr.DownstreamPipelineID)
		}
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE cause = 'upstream'`,
	).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != len(first) {
		t.Fatalf("upstream runs = %d, want %d", total, len(first))
	}
}

func TestFanoutFromStage_NoMatchesReturnsEmpty(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Single-pipeline project with no downstream listeners.
	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	var stageID uuid.UUID
	for _, st := range run.StageRuns {
		if st.Name == "build" {
			stageID = st.ID
		}
	}

	triggered, err := s.FanoutFromStage(ctx, stageID)
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	if len(triggered) != 0 {
		t.Fatalf("expected no downstreams, got %d", len(triggered))
	}
}

func TestFanoutFromStage_IgnoresOtherProjects(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, _, _, coreMat := seedFanoutProject(t, pool)
	_, testStageID := completeUpstreamTestStage(t, pool, coreID, coreMat)

	// A different project adds a downstream-looking pipeline with the same
	// upstream reference. Fanout must not trigger it — scope is per-project.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "other", Name: "Other",
		Pipelines: []*domain.Pipeline{{
			Name:   "hitchhiker",
			Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type:        domain.MaterialUpstream,
				Fingerprint: domain.UpstreamFingerprint("build-core", "test"),
				AutoUpdate:  true,
				Upstream:    &domain.UpstreamMaterial{Pipeline: "build-core", Stage: "test", Status: "success"},
			}},
			Jobs: []domain.Job{{Name: "deploy", Stage: "deploy", Tasks: []domain.Task{{Script: "echo"}}}},
		}},
	}); err != nil {
		t.Fatalf("apply other: %v", err)
	}

	triggered, err := s.FanoutFromStage(ctx, testStageID)
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	if len(triggered) != 2 {
		t.Fatalf("triggered = %d, want 2 (same-project only)", len(triggered))
	}
}
