package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func completeSeedRun(t *testing.T, pool *pgxpool.Pool, s *store.Store, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	agentID := seedAgentRow(t, pool, "terminal-effects-agent")
	var compileID, unitID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='compile'`, runID).Scan(&compileID); err != nil {
		t.Fatalf("compile id: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='unit'`, runID).Scan(&unitID); err != nil {
		t.Fatalf("unit id: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET status='running' WHERE id=$1`, runID); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE stage_runs SET status='running' WHERE run_id=$1 AND name='build'`, runID); err != nil {
		t.Fatalf("mark build stage running: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW() WHERE id=$1`,
		compileID, agentID); err != nil {
		t.Fatalf("mark compile running: %v", err)
	}
	comp, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        compileID,
		Status:          "success",
		ExitCode:        0,
		ExpectedAgentID: agentID,
	})
	if err != nil || !ok {
		t.Fatalf("complete compile: ok=%v err=%v", ok, err)
	}
	if comp.RunCompleted {
		t.Fatalf("compile should not complete run: %+v", comp)
	}
	if _, err := pool.Exec(ctx, `UPDATE stage_runs SET status='running' WHERE run_id=$1 AND name='test'`, runID); err != nil {
		t.Fatalf("mark test stage running: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW() WHERE id=$1`,
		unitID, agentID); err != nil {
		t.Fatalf("mark unit running: %v", err)
	}
	comp, ok, err = s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        unitID,
		Status:          "success",
		ExitCode:        0,
		ExpectedAgentID: agentID,
	})
	if err != nil || !ok {
		t.Fatalf("complete unit: ok=%v err=%v", ok, err)
	}
	if !comp.RunCompleted || comp.RunStatus != "success" {
		t.Fatalf("unit should complete run green: %+v", comp)
	}
}

func TestRunTerminalEffects_CompletionQueuesAndCompletes(t *testing.T) {
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	spy := &spyChecks{}
	sched := scheduler.New(st, sessions, quietLogger(), testDSN).WithChecksReporter(spy)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	completeSeedRun(t, pool, st, runID)

	pending, err := st.ListPendingRunTerminalEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending terminal effects: %v", err)
	}
	if !containsRun(pending, runID) {
		t.Fatalf("run should be pending terminal effects after completion")
	}

	sched.FireRunTerminalEffects(ctx, runID)
	sched.FireRunTerminalEffects(ctx, runID)

	spy.mu.Lock()
	gotChecks := append([]string(nil), spy.completed...)
	spy.mu.Unlock()
	want := runID.String() + ":success"
	if len(gotChecks) != 1 || gotChecks[0] != want {
		t.Fatalf("checks.ReportRunCompleted calls = %v, want [%s]", gotChecks, want)
	}
	var doneAt *time.Time
	var required bool
	if err := pool.QueryRow(ctx, `SELECT terminal_effects_at FROM runs WHERE id=$1`, runID).Scan(&doneAt); err != nil {
		t.Fatalf("read terminal_effects_at: %v", err)
	}
	if doneAt == nil {
		t.Fatalf("terminal effects were not marked done for no-services run")
	}
	if err := pool.QueryRow(ctx, `SELECT terminal_effects_required FROM runs WHERE id=$1`, runID).Scan(&required); err != nil {
		t.Fatalf("read terminal_effects_required: %v", err)
	}
	if required {
		t.Fatalf("terminal effects still marked required after done")
	}
}

func TestRunTerminalEffects_HistoricalTerminalDefaultIsNotReplayed(t *testing.T) {
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		SET status='success', finished_at=NOW(),
		    terminal_effects_required=false,
		    terminal_effects_claimed_at=NULL,
		    terminal_effects_at=NULL
		WHERE id=$1
	`, runID); err != nil {
		t.Fatalf("mark historical terminal: %v", err)
	}

	pending, err := st.ListPendingRunTerminalEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending terminal effects: %v", err)
	}
	if containsRun(pending, runID) {
		t.Fatalf("historical/default terminal row must not be replayed")
	}
}

func TestRunTerminalEffects_ManualCancelClosesCheck(t *testing.T) {
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	spy := &spyChecks{}
	sched := scheduler.New(st, sessions, quietLogger(), testDSN).WithChecksReporter(spy)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	if _, err := st.CancelRun(ctx, runID); err != nil {
		t.Fatalf("cancel run: %v", err)
	}

	pending, err := st.ListPendingRunTerminalEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending terminal effects: %v", err)
	}
	if !containsRun(pending, runID) {
		t.Fatalf("manual cancel should queue terminal effects")
	}

	sched.FireRunTerminalEffects(ctx, runID)

	spy.mu.Lock()
	gotChecks := append([]string(nil), spy.completed...)
	spy.mu.Unlock()
	want := runID.String() + ":canceled"
	if len(gotChecks) != 1 || gotChecks[0] != want {
		t.Fatalf("checks.ReportRunCompleted calls = %v, want [%s]", gotChecks, want)
	}
}

func TestRunTerminalEffects_ServicesNoTargetStaysPending(t *testing.T) {
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(st, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		SET status='canceled', finished_at=NOW(), has_services=true,
		    terminal_effects_required=true,
		    terminal_effects_claimed_at=NULL, terminal_effects_at=NULL
		WHERE id=$1
	`, runID); err != nil {
		t.Fatalf("mark terminal with services: %v", err)
	}

	sched.FireRunTerminalEffects(ctx, runID)

	var doneAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT terminal_effects_at FROM runs WHERE id=$1`, runID).Scan(&doneAt); err != nil {
		t.Fatalf("read terminal_effects_at: %v", err)
	}
	if doneAt != nil {
		t.Fatalf("terminal effects marked done with services + no cleanup target")
	}
}

func TestRunTerminalEffects_StaleGenerationCannotMarkDone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		SET status='failed', finished_at=NOW(), service_generation=7,
		    terminal_effects_required=true,
		    terminal_effects_claimed_at=NULL, terminal_effects_at=NULL
		WHERE id=$1
	`, runID); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}
	claim, claimed, err := st.ClaimRunTerminalEffects(ctx, runID)
	if err != nil || !claimed {
		t.Fatalf("claim terminal effects: claimed=%v err=%v", claimed, err)
	}
	if claim.ServiceGeneration != 7 {
		t.Fatalf("claimed generation = %d, want 7", claim.ServiceGeneration)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		SET status='failed', finished_at=NOW(), service_generation=8,
		    terminal_effects_required=true,
		    terminal_effects_claimed_at=NULL, terminal_effects_at=NULL
		WHERE id=$1
	`, runID); err != nil {
		t.Fatalf("simulate rerun terminalization: %v", err)
	}
	done, err := st.MarkRunTerminalEffectsDone(ctx, runID, claim.ServiceGeneration)
	if err != nil {
		t.Fatalf("mark done with stale generation: %v", err)
	}
	if done {
		t.Fatalf("stale generation marked terminal effects done")
	}
	pending, err := st.ListPendingRunTerminalEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending terminal effects: %v", err)
	}
	if !containsRun(pending, runID) {
		t.Fatalf("new terminal generation should remain pending")
	}
}
