package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestReaper_Sweep_RequeuesOfflineAgentJobs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "reaper-1")

	// Flip the compile job to running with this agent, then mark the agent
	// offline — the reaper should re-queue on the next sweep.
	if _, err := pool.Exec(ctx, `
		UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW()
		WHERE run_id=$2 AND name='compile'`, agentID, runID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("agent offline: %v", err)
	}

	reaper := scheduler.NewReaper(s, quietLogger()).
		WithStaleness(10 * time.Second).
		WithMaxAttempts(3)
	reaper.Sweep(ctx)

	var (
		status     string
		pgAgentID  *uuid.UUID
		attempt    int32
	)
	_ = pool.QueryRow(ctx,
		`SELECT status, agent_id, attempt FROM job_runs WHERE run_id=$1 AND name='compile'`,
		runID,
	).Scan(&status, &pgAgentID, &attempt)
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}
	if pgAgentID != nil {
		t.Fatalf("agent_id not cleared: %v", pgAgentID)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
}

func TestReaper_Sweep_NoStaleJobsIsNoop(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	reaper := scheduler.NewReaper(s, quietLogger())
	// Empty DB — sweep must return without error and not panic.
	reaper.Sweep(context.Background())
}
