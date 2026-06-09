package runs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedRunWithModification is a richer twin of seedRun: it also writes
// a modification row matching the run's revision so Rerun can find
// one to replay from. Returns (run_id, pipeline_id).
func seedRunWithModification(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url, branch := "https://github.com/org/demo", "main"
	fp := domain.GitFingerprint(url, branch)
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo-actions", Name: "Demo Actions",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID
	var matID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID); err != nil {
		t.Fatalf("material lookup: %v", err)
	}

	mod, err := s.InsertModification(ctx, store.Modification{
		MaterialID:  matID,
		Revision:    "cafebabe",
		Branch:      branch,
		Author:      "tester",
		Message:     "feat: hi",
		Payload:     json.RawMessage(`{"ref":"refs/heads/main"}`),
		CommittedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("insert modification: %v", err)
	}

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     matID,
		ModificationID: mod.ID,
		Revision:       "cafebabe",
		Branch:         branch,
		Provider:       "github",
		Delivery:       "t",
		TriggeredBy:    "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run.RunID, pipelineID
}

func TestCancel_Success(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)

	rr := doPost(h, "/api/v1/runs/"+runID.String()+"/cancel", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	// Verify the DB-level effects: run is canceled; no queued stages
	// or jobs remain in 'queued'.
	var runStatus string
	_ = pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id = $1`, runID).Scan(&runStatus)
	if runStatus != "canceled" {
		t.Fatalf("run status = %q", runStatus)
	}
	var queuedStages, queuedJobs int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM stage_runs WHERE run_id = $1 AND status = 'queued'`, runID).Scan(&queuedStages)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM job_runs WHERE run_id = $1 AND status = 'queued'`, runID).Scan(&queuedJobs)
	if queuedStages != 0 || queuedJobs != 0 {
		t.Fatalf("leftover queued: stages=%d jobs=%d", queuedStages, queuedJobs)
	}
}

// fakeDispatcher captures CancelJob pushes so the test can assert
// that the right jobs were signaled on the right agents. Matches the
// narrow CancelDispatcher interface the Handler depends on — keeps
// the test from needing the full grpcsrv stack.
type fakeDispatcher struct {
	calls []dispatchCall
}

type dispatchCall struct {
	agentID uuid.UUID
	runID   string
	jobID   string
}

func (f *fakeDispatcher) Dispatch(agentID uuid.UUID, msg *gocdnextv1.ServerMessage) error {
	c := msg.GetCancel()
	if c == nil {
		return nil
	}
	f.calls = append(f.calls, dispatchCall{
		agentID: agentID,
		runID:   c.GetRunId(),
		jobID:   c.GetJobId(),
	})
	return nil
}

// AllAgentIDs: the cancel path now also broadcasts
// CleanupRunServices to every currently-connected agent (union
// with agents-that-ran-the-run). The test's fake doesn't simulate
// connected agents — returning nil makes the broadcast a no-op,
// which keeps the existing assertions (per-job CancelJob calls)
// focused.
func (*fakeDispatcher) AllAgentIDs(string) []uuid.UUID { return nil }

func TestCancel_DispatchesCancelJobToRunningAgents(t *testing.T) {
	// Regression cover for the cancel-kills-container fix: running
	// jobs assigned to an agent must receive a CancelJob push so the
	// agent can kill its container. Without this, cancel is DB-only
	// and the container keeps burning.
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)

	// Promote the seeded compile job to running + assign it to a
	// fake agent. Seed an agents row first because job_runs.agent_id
	// has an FK.
	agentID := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, 'test-agent', 'x')`,
		agentID)
	if err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	var jobID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`UPDATE job_runs SET status = 'running', agent_id = $1, started_at = NOW()
		 WHERE run_id = $2 AND status = 'queued' RETURNING id`,
		agentID, runID).Scan(&jobID); err != nil {
		t.Fatalf("promote job: %v", err)
	}

	disp := &fakeDispatcher{}
	h = h.WithCancelDispatcher(disp)

	rr := doPost(h, "/api/v1/runs/"+runID.String()+"/cancel", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher called %d times, want 1", len(disp.calls))
	}
	call := disp.calls[0]
	if call.agentID != agentID {
		t.Errorf("dispatched to %s, want %s", call.agentID, agentID)
	}
	if call.jobID != jobID.String() {
		t.Errorf("dispatched job %s, want %s", call.jobID, jobID)
	}
	if call.runID != runID.String() {
		t.Errorf("dispatched run %s, want %s", call.runID, runID)
	}
	// Response body should expose the number of agents signaled so
	// the UI can show an honest "cancel requested" state.
	var body struct {
		SignaledJobs int `json:"signaled_jobs"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.SignaledJobs != 1 {
		t.Errorf("signaled_jobs = %d, want 1", body.SignaledJobs)
	}
}

func TestCancel_NotFound(t *testing.T) {
	h, _ := handler(t)
	rr := doPost(h, "/api/v1/runs/"+uuid.NewString()+"/cancel", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCancel_AlreadyTerminal(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)

	// Flip the run to success so the second cancel hits the 409 path.
	_, _ = pool.Exec(context.Background(),
		`UPDATE runs SET status = 'success', finished_at = NOW() WHERE id = $1`, runID)

	rr := doPost(h, "/api/v1/runs/"+runID.String()+"/cancel", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409", rr.Code)
	}
}

// --- Job-scoped cancel (issue #14) -----------------------------------

func firstJobIDOfRun(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id = $1 LIMIT 1`, runID).Scan(&id); err != nil {
		t.Fatalf("first job_run lookup: %v", err)
	}
	return id
}

// TestCancelJob_QueuedFlipsAndCascades covers the no-agent path: a
// queued job_run gets DB-flipped directly. Because the seeded
// pipeline has exactly one job in the single stage, cancelling it
// is also "last unfinished job in stage" — cascadeAfterJobCompletion
// fires and the stage + run move to canceled too.
func TestCancelJob_QueuedFlipsAndCascades(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	jobID := firstJobIDOfRun(t, pool, runID)

	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/cancel", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var jobStatus string
	_ = pool.QueryRow(context.Background(),
		`SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&jobStatus)
	if jobStatus != "canceled" {
		t.Errorf("job status = %q, want canceled", jobStatus)
	}
	// Cascade: stage + run also terminal (last job in the single-job
	// pipeline). The stage status is `canceled` because the only job
	// in it is canceled; run status derives from the stage outcome.
	var runStatus string
	_ = pool.QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, runID).Scan(&runStatus)
	if runStatus == "queued" || runStatus == "running" {
		t.Errorf("run still active after only-job cancel: %q", runStatus)
	}

	// Response carries run_id + signaled=false (no agent to push to).
	var body struct {
		RunID    string `json:"run_id"`
		Signaled bool   `json:"signaled"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.RunID != runID.String() {
		t.Errorf("run_id in body = %q, want %s", body.RunID, runID)
	}
	if body.Signaled {
		t.Errorf("signaled should be false for queued cancel (no agent)")
	}
	if body.Status != "canceled" {
		t.Errorf("status in body = %q", body.Status)
	}
}

// TestCancelJob_RunningPushesCancelJobFrame covers the agent-held
// case: the row stays 'running' in the DB (agent finalises via
// JobResult), but a single CancelJob frame is pushed down the
// agent's session. Regression cover for the issue #14 root cause:
// pre-fix, cancelling a running job either did nothing or killed
// the whole run.
func TestCancelJob_RunningPushesCancelJobFrame(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	jobID := firstJobIDOfRun(t, pool, runID)

	// Promote the job to running + assign to a fake agent.
	agentID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, 'test-agent-jc', 'x')`,
		agentID); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status = 'running', agent_id = $1, started_at = NOW()
		 WHERE id = $2`, agentID, jobID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	disp := &fakeDispatcher{}
	h = h.WithCancelDispatcher(disp)

	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/cancel", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", len(disp.calls))
	}
	call := disp.calls[0]
	if call.agentID != agentID || call.jobID != jobID.String() {
		t.Errorf("dispatched %+v, want agent=%s job=%s", call, agentID, jobID)
	}

	// DB-side: status stays 'running' until the agent ships JobResult.
	// (Honest audit trail: finished_at reflects when the container
	// actually stopped, not when the user clicked Cancel.)
	var status string
	_ = pool.QueryRow(context.Background(),
		`SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&status)
	if status != "running" {
		t.Errorf("job status after cancel = %q, want still running", status)
	}

	// Run + sibling stages untouched — only this job_run is in flight.
	// Run row may be queued or running depending on whether the
	// scheduler has ticked yet; the load-bearing assertion is that
	// it has NOT been pushed terminal by the job cancel.
	var runStatus string
	_ = pool.QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, runID).Scan(&runStatus)
	if runStatus == "canceled" || runStatus == "failed" || runStatus == "success" {
		t.Errorf("run status = %q, want non-terminal (job-cancel must not touch the run)", runStatus)
	}

	var body struct {
		Signaled bool `json:"signaled"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if !body.Signaled {
		t.Errorf("signaled should be true for running cancel with agent")
	}
}

// failingDispatcher mirrors fakeDispatcher but returns an error
// from Dispatch — simulates "agent disconnected" / "session busy".
// Regression cover for the bug the v0.14.5 first cut shipped:
// dispatch failure with status='running' used to return 202
// status="canceled" even though the container kept running.
type failingDispatcher struct {
	calls []dispatchCall
	err   error
}

func (f *failingDispatcher) Dispatch(agentID uuid.UUID, msg *gocdnextv1.ServerMessage) error {
	c := msg.GetCancel()
	if c != nil {
		f.calls = append(f.calls, dispatchCall{
			agentID: agentID, runID: c.GetRunId(), jobID: c.GetJobId(),
		})
	}
	return f.err
}
func (*failingDispatcher) AllAgentIDs(string) []uuid.UUID { return nil }

// TestCancelJob_DispatchFailureReturns503 — running cancel where
// the gRPC dispatch errors must NOT report success. The job is
// still burning on the agent; the operator needs to know the
// cancel didn't take and retry. Returning 202 status="canceled"
// here would be a lie.
func TestCancelJob_DispatchFailureReturns503(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	jobID := firstJobIDOfRun(t, pool, runID)

	agentID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, 'failing-agent', 'x')`,
		agentID); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status = 'running', agent_id = $1, started_at = NOW()
		 WHERE id = $2`, agentID, jobID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	disp := &failingDispatcher{err: errSimulatedSessionGone}
	h = h.WithCancelDispatcher(disp)

	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/cancel", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Status   string `json:"status"`
		Signaled bool   `json:"signaled"`
		Error    string `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Status != "dispatch_failed" {
		t.Errorf("status = %q, want dispatch_failed", body.Status)
	}
	if body.Signaled {
		t.Errorf("signaled should be false on dispatch failure")
	}
	if body.Error == "" {
		t.Errorf("error message should explain the failure")
	}

	// Critical: the DB state didn't change — job is still running.
	// If we'd flipped it to canceled, the operator's retry would
	// see 409 and never get the chance to actually cancel.
	var status string
	_ = pool.QueryRow(context.Background(),
		`SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&status)
	if status != "running" {
		t.Errorf("job status = %q, want still running after dispatch failure", status)
	}
}

// TestCancelJob_RunningWithNoAgentReturns503 — row marked running
// but agent_id is NULL (transient window between AssignJob committing
// and the session ack landing). No one to dispatch to → 503; the
// operator retries when the agent's session is up.
func TestCancelJob_RunningWithNoAgentReturns503(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	jobID := firstJobIDOfRun(t, pool, runID)

	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status = 'running', agent_id = NULL, started_at = NOW()
		 WHERE id = $1`, jobID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	disp := &fakeDispatcher{}
	h = h.WithCancelDispatcher(disp)

	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/cancel", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	// No dispatch should have happened — there was no agent to push to.
	if len(disp.calls) != 0 {
		t.Errorf("dispatcher calls = %d, want 0 when agent_id is NULL", len(disp.calls))
	}
	_ = runID
}

// errSimulatedSessionGone stands in for the real
// grpcsrv.ErrSessionGone / ErrSessionBusy that the dispatcher would
// surface in production. Decoupled here so the test doesn't import
// grpcsrv just to grab the sentinel.
var errSimulatedSessionGone = stringError("session gone (simulated)")

type stringError string

func (e stringError) Error() string { return string(e) }

// TestCancelJob_NotFound — random UUID → 404.
func TestCancelJob_NotFound(t *testing.T) {
	h, _ := handler(t)
	rr := doPost(h, "/api/v1/job_runs/"+uuid.NewString()+"/cancel", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d want 404", rr.Code)
	}
}

// TestCancelJob_AlreadyTerminal — already-success → 409, idempotent
// for the operator (the click was on a stale UI; nothing new to do).
func TestCancelJob_AlreadyTerminal(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	jobID := firstJobIDOfRun(t, pool, runID)
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status = 'success', finished_at = NOW() WHERE id = $1`, jobID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/cancel", nil)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d want 409", rr.Code)
	}
}

func TestRerun_Success(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)

	rr := doPost(h, "/api/v1/runs/"+runID.String()+"/rerun", []byte(`{"triggered_by":"klinux"}`))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		RunID   string `json:"run_id"`
		Counter int64  `json:"counter"`
		RerunOf string `json:"rerun_of"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.RunID == "" || got.RunID == runID.String() {
		t.Fatalf("new run id missing or equal to orig: %+v", got)
	}
	if got.RerunOf != runID.String() {
		t.Fatalf("rerun_of = %q want %s", got.RerunOf, runID)
	}

	var cause, triggeredBy string
	_ = pool.QueryRow(context.Background(),
		`SELECT cause, COALESCE(triggered_by, '') FROM runs WHERE id = $1`, got.RunID,
	).Scan(&cause, &triggeredBy)
	if cause != "manual" {
		t.Fatalf("cause = %q want manual", cause)
	}
	if triggeredBy != "klinux" {
		t.Fatalf("triggered_by = %q", triggeredBy)
	}
}

func TestRerunJob_ReusesRunAndBumpsAttempt(t *testing.T) {
	// Re-run a single terminal job inside its existing run — should
	// flip the job back to queued, bump attempt, wipe logs, and
	// re-open the parent stage+run so the scheduler picks it up.
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)

	ctx := context.Background()
	// Mark run + stage + job terminal like a real failed run, then
	// drop a log line so we can assert it gets wiped.
	var jobID, stageID uuid.UUID
	if err := pool.QueryRow(ctx,
		`UPDATE job_runs SET status='failed', started_at=NOW()-interval '1m',
		                     finished_at=NOW(), exit_code=1, error='boom'
		 WHERE run_id=$1 RETURNING id, stage_run_id`, runID).
		Scan(&jobID, &stageID); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='failed', finished_at=NOW() WHERE id=$1`, stageID); err != nil {
		t.Fatalf("fail stage: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='failed', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO log_lines (job_run_id, seq, stream, at, text)
		 VALUES ($1, 1, 'stdout', NOW(), 'old attempt output')`, jobID); err != nil {
		t.Fatalf("insert log: %v", err)
	}

	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/rerun", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		JobRunID string `json:"job_run_id"`
		RunID    string `json:"run_id"`
		Attempt  int    `json:"attempt"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.JobRunID != jobID.String() {
		t.Errorf("job_run_id = %q, want %s", body.JobRunID, jobID)
	}
	if body.RunID != runID.String() {
		t.Errorf("run_id = %q, want %s (same run, not a new one)", body.RunID, runID)
	}
	if body.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (was 0 pre-rerun)", body.Attempt)
	}

	// Job back to queued, run+stage back to running.
	var jobStatus, stageStatus, runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&jobStatus)
	_ = pool.QueryRow(ctx, `SELECT status FROM stage_runs WHERE id=$1`, stageID).Scan(&stageStatus)
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if jobStatus != "queued" {
		t.Errorf("job_run status = %q, want queued", jobStatus)
	}
	if stageStatus != "running" {
		t.Errorf("stage status = %q, want running (un-finished)", stageStatus)
	}
	if runStatus != "running" {
		t.Errorf("run status = %q, want running (un-finished)", runStatus)
	}

	// Old attempt's logs should be gone.
	var logCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM log_lines WHERE job_run_id=$1`, jobID).Scan(&logCount)
	if logCount != 0 {
		t.Errorf("log_lines = %d, want 0 (previous attempt should be wiped)", logCount)
	}
}

func TestRerunJob_RefusesActiveJob(t *testing.T) {
	// Rerunning a job that's still queued or running would double-
	// schedule it. Operator has to cancel first.
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	var jobID uuid.UUID
	_ = pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id=$1 LIMIT 1`, runID).Scan(&jobID)

	// Job starts as 'queued' from the seed; hit the rerun endpoint.
	rr := doPost(h, "/api/v1/job_runs/"+jobID.String()+"/rerun", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409, body=%s", rr.Code, rr.Body.String())
	}
}

func TestRerunJob_NotFound(t *testing.T) {
	h, _ := handler(t)
	rr := doPost(h, "/api/v1/job_runs/"+uuid.NewString()+"/rerun", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404", rr.Code)
	}
}

func TestRerunJob_InvalidID(t *testing.T) {
	h, _ := handler(t)
	rr := doPost(h, "/api/v1/job_runs/not-a-uuid/rerun", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", rr.Code)
	}
}

func TestRerun_InvalidBody(t *testing.T) {
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)

	rr := doPost(h, "/api/v1/runs/"+runID.String()+"/rerun", []byte(`{not json`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestRerun_NotFound(t *testing.T) {
	h, _ := handler(t)
	rr := doPost(h, "/api/v1/runs/"+uuid.NewString()+"/rerun", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestTriggerPipeline_Success(t *testing.T) {
	h, pool := handler(t)
	_, pipelineID := seedRunWithModification(t, pool)

	rr := doPost(h, "/api/v1/pipelines/"+pipelineID.String()+"/trigger", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		RunID      string `json:"run_id"`
		Counter    int64  `json:"counter"`
		PipelineID string `json:"pipeline_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.RunID == "" {
		t.Fatalf("run_id missing: %+v", got)
	}
	if got.PipelineID != pipelineID.String() {
		t.Fatalf("pipeline_id = %q", got.PipelineID)
	}
}

func TestTriggerPipeline_NoModifications(t *testing.T) {
	// Git-backed pipeline that has never seen a push. Fetcher isn't
	// wired on this handler, so the seed fallback can't run and the
	// 422 hint surfaces unchanged.
	h, pool := handler(t)
	s := store.New(pool)
	ctx := context.Background()
	url, branch := "https://github.com/org/never-pushed", "main"
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "never-pushed", Name: "never-pushed",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: domain.GitFingerprint(url, branch),
				AutoUpdate: true,
				Git:        &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID

	rr := doPost(h, "/api/v1/pipelines/"+pipelineID.String()+"/trigger", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d want 422, body=%s", rr.Code, rr.Body.String())
	}
}

func TestTriggerPipeline_UpstreamOnly(t *testing.T) {
	// Pipelines whose only material is `upstream:` (typical for
	// "ci-web depends on ci-server.test" fanout) have no git to
	// seed from and no modifications until an upstream run
	// succeeds. Manual trigger must still work — insert a bare
	// run skeleton so operators can kick the downstream without
	// waiting for the upstream to land.
	h, pool := handler(t)
	s := store.New(pool)
	ctx := context.Background()
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "web-only", Name: "web-only",
		Pipelines: []*domain.Pipeline{{
			Name:   "downstream",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialUpstream,
				Fingerprint: domain.UpstreamFingerprint("ci-server", "test"),
				AutoUpdate:  true,
				Upstream:    &domain.UpstreamMaterial{Pipeline: "ci-server", Stage: "test", Status: "success"},
			}},
			Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID

	rr := doPost(h, "/api/v1/pipelines/"+pipelineID.String()+"/trigger", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d want 202, body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.RunID == "" {
		t.Fatalf("run_id missing: %s", rr.Body.String())
	}
}

func TestTriggerPipeline_InvalidID(t *testing.T) {
	h, _ := handler(t)
	rr := doPost(h, "/api/v1/pipelines/not-a-uuid/trigger", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

// --- helpers ---

func doPost(h interface {
	Cancel(http.ResponseWriter, *http.Request)
	CancelJob(http.ResponseWriter, *http.Request)
	Rerun(http.ResponseWriter, *http.Request)
	RerunJob(http.ResponseWriter, *http.Request)
	TriggerPipeline(http.ResponseWriter, *http.Request)
}, path string, body []byte) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Post("/api/v1/runs/{id}/cancel", h.Cancel)
	r.Post("/api/v1/runs/{id}/rerun", h.Rerun)
	r.Post("/api/v1/job_runs/{id}/cancel", h.CancelJob)
	r.Post("/api/v1/job_runs/{id}/rerun", h.RerunJob)
	r.Post("/api/v1/pipelines/{id}/trigger", h.TriggerPipeline)

	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}
