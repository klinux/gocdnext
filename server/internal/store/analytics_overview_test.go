package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestAnalyticsOverview_CurrentVsPriorAndSeries(t *testing.T) {
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

	// Window = 7 days. Current window [0,7): 3 successes + 1 failed.
	// Prior window [7,14): 1 success only — so deltas are non-zero.
	seedDeploy(t, pool, ctx, envID, pipelineID, 1, "success", false, 1, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 2, "success", false, 2, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 3, "success", false, 4, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 4, "failed", false, 3, 10)
	seedDeploy(t, pool, ctx, envID, pipelineID, 5, "success", false, 9, 10) // prior

	ov, err := s.AnalyticsOverview(ctx, "team", 7)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if ov.Key != "team" || ov.WindowDays != 7 {
		t.Fatalf("echo = %+v", ov)
	}
	if ov.Current.DeploysSuccess != 3 || ov.Current.DeploysTotal != 4 || ov.Current.DeploysFailed != 1 {
		t.Fatalf("current = %+v", ov.Current)
	}
	if ov.Prior.DeploysSuccess != 1 || ov.Prior.DeploysTotal != 1 {
		t.Fatalf("prior = %+v", ov.Prior)
	}
	// CFR current = 1/4, prior = 0; freq current = 3/7, prior = 1/7.
	if math.Abs(ov.Current.ChangeFailureRate-0.25) > 1e-6 {
		t.Errorf("current CFR = %v, want 0.25", ov.Current.ChangeFailureRate)
	}
	if math.Abs(ov.Current.DeployFreqPerDay-3.0/7.0) > 1e-6 {
		t.Errorf("current freq = %v, want 3/7", ov.Current.DeployFreqPerDay)
	}
	if math.Abs(ov.Prior.DeployFreqPerDay-1.0/7.0) > 1e-6 {
		t.Errorf("prior freq = %v, want 1/7", ov.Prior.DeployFreqPerDay)
	}
	// Lead time p50 on the successes = 10 min = 600s.
	if math.Abs(ov.Current.LeadTimeP50Sec-600) > 1 {
		t.Errorf("lead = %v, want ~600", ov.Current.LeadTimeP50Sec)
	}

	// Daily series: 4 distinct days carried a terminal deploy in the window.
	if len(ov.Daily) != 4 {
		t.Fatalf("daily days = %d (%+v), want 4", len(ov.Daily), ov.Daily)
	}
	var seriesTotal int64
	for _, d := range ov.Daily {
		if d.Day == "" {
			t.Errorf("daily day empty: %+v", d)
		}
		seriesTotal += d.DeploysTotal
	}
	if seriesTotal != 4 {
		t.Errorf("series total = %d, want 4 (current window only)", seriesTotal)
	}

	// Leaderboard carries the one team.
	if len(ov.Teams) != 1 || ov.Teams[0].Group != "payments" {
		t.Fatalf("teams = %+v", ov.Teams)
	}
}
