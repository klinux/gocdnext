package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedDeploy inserts a deployment_revision finished `daysAgo` days ago, with a
// producing run STARTED `leadMin` minutes before the deploy finished (so lead
// time = leadMin minutes for a success). created_at is 2 min earlier still, to
// prove the queue wait is excluded from lead time.
func seedDeploy(t *testing.T, pool *pgxpool.Pool, ctx context.Context, envID, pipelineID uuid.UUID, counter int, status string, isRollback bool, daysAgo, leadMin int) {
	t.Helper()
	var runID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO runs (pipeline_id, counter, cause, status, revisions, created_at, started_at, finished_at)
		VALUES ($1, $2, 'manual', 'success', '{}'::jsonb,
		        now() - make_interval(days => $3, mins => $4 + 2),
		        now() - make_interval(days => $3, mins => $4),
		        now() - make_interval(days => $3))
		RETURNING id`,
		pipelineID, counter, daysAgo, leadMin).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_revisions (environment_id, run_id, version, status, is_rollback, finished_at)
		VALUES ($1, $2, $3, $4, $5, now() - make_interval(days => $6))`,
		envID, runID, status+"-ver", status, isRollback, daysAgo); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
}

func TestDoraRollup_Metrics(t *testing.T) {
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

	// failed at 3d ago; success at 2d ago (the restore → MTTR ≈ 1 day);
	// successes at 1d and ~now. lead time = 10 min on the successes.
	seedDeploy(t, pool, ctx, envID, pipelineID, 1, "failed", false, 3, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 2, "success", false, 2, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 3, "success", false, 1, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 4, "success", false, 0, 10)

	groups, err := s.DoraRollup(ctx, "team", 30, "")
	if err != nil {
		t.Fatalf("dora: %v", err)
	}
	if len(groups) != 1 || groups[0].Group != "payments" {
		t.Fatalf("groups = %+v", groups)
	}
	g := groups[0]
	if g.DeploysSuccess != 3 || g.DeploysTotal != 4 || g.DeploysFailed != 1 {
		t.Fatalf("counts = %+v", g)
	}
	// CFR = 1/4.
	if math.Abs(g.ChangeFailureRate-0.25) > 1e-6 {
		t.Errorf("CFR = %v, want 0.25", g.ChangeFailureRate)
	}
	// Deployment frequency = 3 successes / 30 days.
	if math.Abs(g.DeployFreqPerDay-3.0/30.0) > 1e-6 {
		t.Errorf("freq = %v, want 0.1", g.DeployFreqPerDay)
	}
	// Lead time median = 10 min = 600s.
	if math.Abs(g.LeadTimeP50Sec-600) > 1 {
		t.Errorf("lead time = %v, want ~600", g.LeadTimeP50Sec)
	}
	// MTTR median = ~1 day = 86400s (failed 3d ago → next success 2d ago).
	if math.Abs(g.MTTRP50Sec-86400) > 5 {
		t.Errorf("MTTR = %v, want ~86400", g.MTTRP50Sec)
	}

	// Window excludes old deploys: a 1-day window sees only the near-now success.
	narrow, err := s.DoraRollup(ctx, "team", 1, "")
	if err != nil {
		t.Fatalf("dora narrow: %v", err)
	}
	if len(narrow) != 1 || narrow[0].DeploysTotal != 1 || narrow[0].DeploysSuccess != 1 {
		t.Fatalf("narrow window = %+v", narrow)
	}
}

func TestLabelKeys(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()
	p, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "a", Name: "a"})
	if err := s.ReplaceProjectLabels(ctx, p.ProjectID, []store.ProjectLabel{
		{Key: "team", Value: "x"}, {Key: "tier", Value: "y"},
	}); err != nil {
		t.Fatalf("labels: %v", err)
	}
	keys, err := s.LabelKeys(ctx)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "team" || keys[1] != "tier" {
		t.Fatalf("keys = %v", keys)
	}
}
