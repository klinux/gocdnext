package store

// Internal test (package store) — reuses gateFixture from supersede_core_test.go.

import (
	"testing"

	"github.com/google/uuid"
)

// #207: the supersede terminalizer stamps its victims' RUNNING jobs with
// cancel_origin='supersede' (so a later rerun revives them — they're a system
// cancel, not a user's), and fans out a CancelJob frame only to agent-owned rows.
func TestSupersede_StampsRunningJobWithSupersedeOrigin(t *testing.T) {
	f := newGateFixture(t, "supersede-origin")
	victim := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	var compileID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='compile'`, victim.RunID).Scan(&compileID); err != nil {
		t.Fatalf("compile id: %v", err)
	}
	agentID := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW() WHERE id=$1`, compileID, agentID); err != nil {
		t.Fatalf("set running: %v", err)
	}

	victims, err := f.runSupersedeE(newer, "main", []string{"staging"})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(victims) != 1 {
		t.Fatalf("victims = %d, want 1", len(victims))
	}

	// Running victim job: stays running, stamped, origin supersede.
	var status, origin string
	var stamp *string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, COALESCE(cancel_origin,''), cancel_requested_at::text FROM job_runs WHERE id=$1`,
		compileID).Scan(&status, &origin, &stamp); err != nil {
		t.Fatalf("read compile: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running (agent finalises it)", status)
	}
	if origin != "supersede" {
		t.Errorf("cancel_origin = %q, want supersede", origin)
	}
	if stamp == nil {
		t.Error("cancel_requested_at not stamped")
	}
	// Agent-owned → present in the fanout.
	found := false
	for _, j := range victims[0].RunningJobs {
		if j.JobID == compileID {
			found = true
			if j.AgentID != agentID {
				t.Errorf("fanout agent = %s, want %s", j.AgentID, agentID)
			}
		}
	}
	if !found {
		t.Error("agent-owned running job missing from the CancelJob fanout")
	}
}

// A NATIVE (agent_id NULL) running victim job is stamped with origin supersede but
// NOT fanned out — the watcher/reaper drives it.
func TestSupersede_NativeRunningStampedNotFannedOut(t *testing.T) {
	f := newGateFixture(t, "supersede-native")
	victim := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	var compileID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='compile'`, victim.RunID).Scan(&compileID); err != nil {
		t.Fatalf("compile id: %v", err)
	}
	// Server-managed native running job: agent_id NULL.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=NULL, started_at=NOW() WHERE id=$1`, compileID); err != nil {
		t.Fatalf("set native running: %v", err)
	}

	victims, err := f.runSupersedeE(newer, "main", []string{"staging"})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(victims) != 1 {
		t.Fatalf("victims = %d, want 1", len(victims))
	}

	var origin string
	var stamp *string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COALESCE(cancel_origin,''), cancel_requested_at::text FROM job_runs WHERE id=$1`,
		compileID).Scan(&origin, &stamp); err != nil {
		t.Fatalf("read compile: %v", err)
	}
	if stamp == nil {
		t.Error("native running job not stamped (agent_id filter must be gone)")
	}
	if origin != "supersede" {
		t.Errorf("cancel_origin = %q, want supersede", origin)
	}
	for _, j := range victims[0].RunningJobs {
		if j.JobID == compileID {
			t.Error("native (agent_id NULL) job must NOT be in the CancelJob fanout")
		}
	}
}
