package cron_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/cron"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestTicker_FiresOverdueSchedule applies a pipeline with a cron
// material whose expression matches every minute, then runs the
// ticker once. A run must land in the `runs` table tagged with
// cause="cron" and the cron_state row must record the fire.
func TestTicker_FiresOverdueSchedule(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "cronproj", Name: "CronProj",
		Pipelines: []*domain.Pipeline{{
			Name:   "nightly",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialCron,
				Fingerprint: domain.CronFingerprint("* * * * *"),
				AutoUpdate:  true,
				Cron:        &domain.CronMaterial{Expression: "* * * * *"},
			}},
			Jobs: []domain.Job{{
				Name: "run", Stage: "build", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo tick"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	// Ticker evaluates with its internal now — give it a short
	// tick then cancel after we see the run land. Priming happens
	// before the ticker's first sleep, so Run() starts firing
	// immediately.
	ticker := cron.New(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(50 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	// Poll the runs table — a fire should produce a run row in
	// under one tick + db round-trip.
	deadline := time.Now().Add(2 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM runs WHERE pipeline_id = $1 AND cause = 'cron'`,
			pipelineID,
		).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count > 0 {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	cancel()
	<-done

	if count == 0 {
		t.Fatalf("expected at least one cron-triggered run")
	}

	// cron_state row must reflect the fire.
	var fired time.Time
	err = pool.QueryRow(ctx, `
		SELECT cs.last_fired_at FROM cron_state cs
		JOIN materials m ON m.id = cs.material_id
		WHERE m.pipeline_id = $1`, pipelineID,
	).Scan(&fired)
	if err != nil {
		t.Fatalf("cron_state lookup: %v", err)
	}
	if fired.IsZero() {
		t.Fatal("cron_state.last_fired_at is zero")
	}
}
