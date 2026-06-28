package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedCorrelatedDeploy wires a full deploy→PR chain: a success run whose
// revisions carry the deployed commit SHA, a deploy job_run with start/finish,
// a deployment_revisions row linking it, and a PR whose merge_sha == the SHA.
func seedCorrelatedDeploy(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	envID, pipelineID uuid.UUID, counter int, sha string, isRollback bool,
	deployStarted, deployFinished time.Time) {
	t.Helper()
	matID := uuid.New().String()
	revisions := `{"` + matID + `": {"revision": "` + sha + `", "branch": "main"}}`

	var runID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO runs (pipeline_id, counter, cause, status, revisions, created_at, started_at, finished_at)
		VALUES ($1, $2, 'webhook', 'success', $3::jsonb, $4, $5, $6) RETURNING id`,
		pipelineID, counter, revisions, deployStarted.Add(-time.Minute), deployStarted, deployFinished,
	).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var stageID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO stage_runs (run_id, name, ordinal, status, started_at, finished_at)
		VALUES ($1, 'deploy', 0, 'success', $2, $3) RETURNING id`,
		runID, deployStarted, deployFinished,
	).Scan(&stageID); err != nil {
		t.Fatalf("seed stage_run: %v", err)
	}

	var jobID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO job_runs (run_id, stage_run_id, name, status, started_at, finished_at)
		VALUES ($1, $2, 'ship', 'success', $3, $4) RETURNING id`,
		runID, stageID, deployStarted, deployFinished,
	).Scan(&jobID); err != nil {
		t.Fatalf("seed job_run: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_revisions (environment_id, run_id, job_run_id, version, status, is_rollback, finished_at)
		VALUES ($1, $2, $3, $4, 'success', $5, $6)`,
		envID, runID, jobID, sha, isRollback, deployFinished); err != nil {
		t.Fatalf("seed deployment_revision: %v", err)
	}
}

func TestDoraBottleneck_Decomposition(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "pay", Name: "pay",
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"deploy"},
			Jobs: []domain.Job{{Name: "ship", Stage: "deploy"}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := s.ReplaceProjectLabels(ctx, res.ProjectID, []store.ProjectLabel{{Key: "team", Value: "payments"}}); err != nil {
		t.Fatalf("labels: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, res.ProjectID, "prod")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	pipelineID := uuid.UUID(res.Pipelines[0].PipelineID)

	// Anchor the chain so the four stages have clean durations:
	// Coding 1h, Review 2h, Release wait 3h, Deploy 20m.
	deployFinished := time.Now().UTC().Add(-24 * time.Hour)
	deployStarted := deployFinished.Add(-20 * time.Minute)
	approved := deployStarted.Add(-3 * time.Hour)
	merged := approved.Add(30 * time.Minute)
	opened := approved.Add(-2 * time.Hour)
	firstCommit := opened.Add(-1 * time.Hour)

	const sha = "deadbeefcafe"
	const repo = "acme/web"
	_ = s.RecordPullRequestOpened(ctx, "github", repo, 7, opened, "t", "a", "feat", "main", "head")
	_ = s.RecordPullRequestFirstCommit(ctx, "github", repo, 7, firstCommit)
	_ = s.RecordPullRequestApproved(ctx, "github", repo, 7, approved)
	_ = s.RecordPullRequestMerged(ctx, "github", repo, 7, merged, sha)

	seedCorrelatedDeploy(t, pool, ctx, envID, pipelineID, 1, sha, false, deployStarted, deployFinished)

	ov, err := s.AnalyticsOverview(ctx, "team", 30, "")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	b := ov.Bottleneck
	if b.Correlated != 1 || b.Excluded != 0 {
		t.Fatalf("counts = %+v", b)
	}
	if b.CodingSample != 1 || b.ReviewSample != 1 || b.ReleaseSample != 1 || b.DeploySample != 1 {
		t.Fatalf("per-stage samples = %+v", b)
	}
	near := func(name string, got, want float64) {
		if math.Abs(got-want) > 2 {
			t.Errorf("%s p50 = %v, want ~%v", name, got, want)
		}
	}
	near("coding", b.CodingP50Sec, 3600)
	near("review", b.ReviewP50Sec, 7200)
	near("release", b.ReleaseP50Sec, 3*3600)
	near("deploy", b.DeployP50Sec, 1200)
	// True end-to-end p50 = deploy_finished − first_commit = 6h20m (here it
	// happens to equal the stage sum, since there's a single deploy).
	near("total", b.TotalP50Sec, 6*3600+20*60)
}

func TestDoraBottleneck_ExcludesUncorrelated(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	res, _ := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "pay", Name: "pay",
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"deploy"},
			Jobs: []domain.Job{{Name: "ship", Stage: "deploy"}},
		}},
	})
	_ = s.ReplaceProjectLabels(ctx, res.ProjectID, []store.ProjectLabel{{Key: "team", Value: "payments"}})
	envID, _ := s.EnsureEnvironment(ctx, res.ProjectID, "prod")
	pipelineID := uuid.UUID(res.Pipelines[0].PipelineID)

	// A success deploy whose SHA matches no PR → counted as excluded, not in p50.
	df := time.Now().UTC().Add(-24 * time.Hour)
	seedCorrelatedDeploy(t, pool, ctx, envID, pipelineID, 1, "orphansha", false, df.Add(-5*time.Minute), df)

	ov, err := s.AnalyticsOverview(ctx, "team", 30, "")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Bottleneck.Correlated != 0 || ov.Bottleneck.Excluded != 1 {
		t.Fatalf("want correlated 0 / excluded 1, got %+v", ov.Bottleneck)
	}
}

func TestDoraBottleneck_ExcludesRollbackAndDedupesPR(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	res, _ := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "pay", Name: "pay",
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"deploy"},
			Jobs: []domain.Job{{Name: "ship", Stage: "deploy"}},
		}},
	})
	_ = s.ReplaceProjectLabels(ctx, res.ProjectID, []store.ProjectLabel{{Key: "team", Value: "payments"}})
	envID, _ := s.EnsureEnvironment(ctx, res.ProjectID, "prod")
	pipelineID := uuid.UUID(res.Pipelines[0].PipelineID)

	now := time.Now().UTC()
	const sha = "sharedsha"
	// TWO PRs share the same merge SHA (mirrored repos) — a deploy must dedupe
	// to one, not fan out to two.
	_ = s.RecordPullRequestOpened(ctx, "github", "acme/web", 7, now.Add(-50*time.Hour), "t", "a", "f", "main", "h")
	_ = s.RecordPullRequestMerged(ctx, "github", "acme/web", 7, now.Add(-49*time.Hour), sha)
	_ = s.RecordPullRequestOpened(ctx, "github", "mirror/web", 8, now.Add(-48*time.Hour), "t", "a", "f", "main", "h")
	_ = s.RecordPullRequestMerged(ctx, "github", "mirror/web", 8, now.Add(-47*time.Hour), sha)

	// One real deploy of that SHA, plus a successful ROLLBACK of the same SHA.
	df := now.Add(-24 * time.Hour)
	seedCorrelatedDeploy(t, pool, ctx, envID, pipelineID, 1, sha, false, df.Add(-10*time.Minute), df)
	seedCorrelatedDeploy(t, pool, ctx, envID, pipelineID, 2, sha, true, df.Add(time.Hour), df.Add(70*time.Minute))

	ov, err := s.AnalyticsOverview(ctx, "team", 30, "")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	// Rollback dropped entirely; the real deploy dedupes to a single PR.
	if ov.Bottleneck.Correlated != 1 || ov.Bottleneck.Excluded != 0 {
		t.Fatalf("want correlated 1 / excluded 0 (rollback & dupe handled), got %+v", ov.Bottleneck)
	}
}

func TestDoraBottleneck_CountsRetentionPrunedAsExcluded(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	res, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "pay", Name: "pay"})
	_ = s.ReplaceProjectLabels(ctx, res.ProjectID, []store.ProjectLabel{{Key: "team", Value: "payments"}})
	envID, _ := s.EnsureEnvironment(ctx, res.ProjectID, "prod")

	// A successful deploy whose run was retention-pruned (run_id/job_run_id NULL)
	// must still be counted — as excluded — not vanish from the universe.
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_revisions (environment_id, run_id, job_run_id, version, status, is_rollback, finished_at)
		VALUES ($1, NULL, NULL, 'v', 'success', false, now() - interval '2 days')`,
		envID); err != nil {
		t.Fatalf("seed pruned deploy: %v", err)
	}

	ov, err := s.AnalyticsOverview(ctx, "team", 30, "")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Bottleneck.Correlated != 0 || ov.Bottleneck.Excluded != 1 {
		t.Fatalf("want correlated 0 / excluded 1 (pruned counts), got %+v", ov.Bottleneck)
	}
}
