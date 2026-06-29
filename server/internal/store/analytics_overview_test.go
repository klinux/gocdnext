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

func TestAnalyticsOverview_EnvironmentFilter(t *testing.T) {
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
	prod, err := s.EnsureEnvironment(ctx, res.ProjectID, "prod")
	if err != nil {
		t.Fatalf("prod env: %v", err)
	}
	stg, err := s.EnsureEnvironment(ctx, res.ProjectID, "staging")
	if err != nil {
		t.Fatalf("staging env: %v", err)
	}
	pipelineID := uuid.UUID(res.Pipelines[0].PipelineID)

	// 2 prod successes, 1 staging success — all within a 30-day window.
	seedDeploy(t, pool, ctx, prod, pipelineID, 1, "success", false, 1, 10)
	seedDeploy(t, pool, ctx, prod, pipelineID, 2, "success", false, 2, 10)
	seedDeploy(t, pool, ctx, stg, pipelineID, 3, "success", false, 1, 10)

	if err := s.RefreshDeployDaily(ctx, 0); err != nil {
		t.Fatalf("refresh deploy: %v", err)
	}

	all, err := s.AnalyticsOverview(ctx, "team", 30, "")
	if err != nil {
		t.Fatalf("overview all: %v", err)
	}
	if all.Current.DeploysSuccess != 3 {
		t.Errorf("all envs success = %d, want 3", all.Current.DeploysSuccess)
	}

	onlyProd, err := s.AnalyticsOverview(ctx, "team", 30, "prod")
	if err != nil {
		t.Fatalf("overview prod: %v", err)
	}
	if onlyProd.Environment != "prod" || onlyProd.Current.DeploysSuccess != 2 {
		t.Errorf("prod = env %q success %d, want prod/2", onlyProd.Environment, onlyProd.Current.DeploysSuccess)
	}

	if onlyStg, err := s.AnalyticsOverview(ctx, "team", 30, "staging"); err != nil {
		t.Fatalf("overview staging: %v", err)
	} else if onlyStg.Current.DeploysSuccess != 1 {
		t.Errorf("staging success = %d, want 1", onlyStg.Current.DeploysSuccess)
	}

	envs, err := s.Environments(ctx, "team")
	if err != nil {
		t.Fatalf("environments: %v", err)
	}
	if len(envs) != 2 || envs[0] != "prod" || envs[1] != "staging" {
		t.Fatalf("environments = %v, want [prod staging]", envs)
	}
}

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

	if err := s.RefreshDeployDaily(ctx, 0); err != nil {
		t.Fatalf("refresh deploy: %v", err)
	}

	ov, err := s.AnalyticsOverview(ctx, "team", 7, "")
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

	// Daily series is dense: exactly the calendar days the counts cover (zero-
	// filled), so a 7-day window yields 7 buckets — same day set as the rollup.
	if len(ov.Daily) != 7 {
		t.Fatalf("daily days = %d (%+v), want 7 (dense)", len(ov.Daily), ov.Daily)
	}
	var seriesTotal, seriesSuccess int64
	for _, d := range ov.Daily {
		if d.Day == "" {
			t.Errorf("daily day empty: %+v", d)
		}
		seriesTotal += d.DeploysTotal
		seriesSuccess += d.DeploysSuccess
	}
	if seriesTotal != 4 || seriesSuccess != 3 {
		t.Errorf("series total/success = %d/%d, want 4/3 (current window only)", seriesTotal, seriesSuccess)
	}

	// Window coherence (#131 review): counts (rollup, calendar-day) and lead
	// time (live) must cover the SAME days. A deploy finished ~24h ago lands on
	// yesterday's bucket; with window_days=1 (today only) BOTH the counts and the
	// lead time must exclude it — never counts=0 with lead>0.
	day1, err := s.AnalyticsOverview(ctx, "team", 1, "")
	if err != nil {
		t.Fatalf("overview day1: %v", err)
	}
	if day1.Current.DeploysTotal != 0 {
		t.Fatalf("1-day window should exclude the ≥1d-old deploys: %+v", day1.Current)
	}
	if day1.Current.LeadTimeP50Sec != 0 {
		t.Errorf("lead must share the counts' calendar window (coherent), got %v with 0 deploys", day1.Current.LeadTimeP50Sec)
	}

	// Leaderboard carries the one team.
	if len(ov.Teams) != 1 || ov.Teams[0].Group != "payments" {
		t.Fatalf("teams = %+v", ov.Teams)
	}
	// TeamsPrior covers the prior window [7,14): the lone daysAgo=9 success.
	if len(ov.TeamsPrior) != 1 || ov.TeamsPrior[0].DeploysSuccess != 1 {
		t.Fatalf("teams_prior = %+v, want payments/1 success", ov.TeamsPrior)
	}
}
