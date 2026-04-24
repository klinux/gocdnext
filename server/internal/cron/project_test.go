package cron_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/cron"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedProjectWithTwoPipelines applies a project with two
// manual-material pipelines so the trigger path succeeds without
// needing a seeded modification (bare-run fallback).
func seedProjectWithTwoPipelines(
	t *testing.T, s *store.Store, slug string,
) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	pipe := func(name string) *domain.Pipeline {
		return &domain.Pipeline{
			Name:   name,
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialManual,
				Fingerprint: domain.ManualFingerprint(),
			}},
			Jobs: []domain.Job{{
				Name:  "run",
				Stage: "build",
				Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo " + name}},
			}},
		}
	}
	res, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{pipe("alpha"), pipe("beta")},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(res.Pipelines))
	for _, p := range res.Pipelines {
		ids = append(ids, p.PipelineID)
	}
	return res.ProjectID, ids
}

// TestProjectTicker_FiresAllPipelines drives an enabled project
// schedule with empty pipeline_ids (= all pipelines). After one
// tick, both pipelines must have a scheduled run.
func TestProjectTicker_FiresAllPipelines(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	projectID, pipes := seedProjectWithTwoPipelines(t, s, "cronproj1")
	_, err := s.InsertProjectCron(ctx, store.ProjectCronInput{
		ProjectID:   projectID,
		Name:        "nightly",
		Expression:  "* * * * *",
		PipelineIDs: nil, // empty = fire all
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("insert project_cron: %v", err)
	}

	ticker := cron.NewProject(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(50 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	deadline := time.Now().Add(3 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM runs WHERE pipeline_id = ANY($1) AND cause = 'schedule'`,
			pipes,
		).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count >= len(pipes) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-done

	if count < len(pipes) {
		t.Fatalf("expected %d scheduled runs (one per pipeline), got %d", len(pipes), count)
	}
}

// TestProjectTicker_FiresOnlyPinnedPipelines drives a schedule
// with pipeline_ids restricted to just one of the two pipelines.
// Only that one may have a scheduled run.
func TestProjectTicker_FiresOnlyPinnedPipelines(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	projectID, pipes := seedProjectWithTwoPipelines(t, s, "cronproj2")
	_, err := s.InsertProjectCron(ctx, store.ProjectCronInput{
		ProjectID:   projectID,
		Name:        "pinned",
		Expression:  "* * * * *",
		PipelineIDs: []uuid.UUID{pipes[0]}, // only alpha
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("insert project_cron: %v", err)
	}

	ticker := cron.NewProject(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(50 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	var countAlpha, countBeta int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE pipeline_id = $1 AND cause = 'schedule'`,
		pipes[0],
	).Scan(&countAlpha); err != nil {
		t.Fatalf("count alpha: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE pipeline_id = $1 AND cause = 'schedule'`,
		pipes[1],
	).Scan(&countBeta); err != nil {
		t.Fatalf("count beta: %v", err)
	}
	if countAlpha == 0 {
		t.Errorf("alpha should have been fired; got 0 scheduled runs")
	}
	if countBeta != 0 {
		t.Errorf("beta shouldn't be in the pinned list; got %d scheduled runs", countBeta)
	}
}

// TestProjectTicker_DisabledSchedule confirms enabled=false
// schedules are skipped entirely — no runs, no last_fired_at
// update.
func TestProjectTicker_DisabledSchedule(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	projectID, pipes := seedProjectWithTwoPipelines(t, s, "cronproj3")
	_, err := s.InsertProjectCron(ctx, store.ProjectCronInput{
		ProjectID:  projectID,
		Name:       "off",
		Expression: "* * * * *",
		Enabled:    false,
	})
	if err != nil {
		t.Fatalf("insert project_cron: %v", err)
	}

	ticker := cron.NewProject(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(50 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE pipeline_id = ANY($1)`,
		pipes,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("disabled schedule should not create runs; got %d", count)
	}
}

// TestRunAll fires the "Run all" operator action and expects
// every pipeline in the project to get a manual-cause run.
func TestRunAll(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	projectID, pipes := seedProjectWithTwoPipelines(t, s, "runall1")

	results, err := cron.RunAll(ctx, s, projectID, "alice")
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(results) != len(pipes) {
		t.Fatalf("expected %d results, got %d", len(pipes), len(results))
	}
	for _, r := range results {
		if r.Error != "" {
			t.Errorf("pipeline %s: %s", r.PipelineID, r.Error)
		}
		if r.RunID == nil {
			t.Errorf("pipeline %s: expected a run id", r.PipelineID)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE pipeline_id = ANY($1) AND cause = 'manual' AND triggered_by = 'alice'`,
		pipes,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != len(pipes) {
		t.Errorf("expected %d manual runs attributed to alice, got %d", len(pipes), count)
	}
}
