package grpcsrv_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const heartbeatSecs = 30

func TestRegister_UnknownAgent(t *testing.T) {
	_, client := bootServer(t)

	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "ghost",
		Token:   "whatever",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", code)
	}
}

func TestRegister_WrongToken(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-01", store.HashToken("right"))

	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-01",
		Token:   "wrong",
	})
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", code)
	}
}

func TestRegister_Succeeds(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-01", store.HashToken("tok"))

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId:  "runner-01",
		Token:    "tok",
		Version:  "0.1.0",
		Os:       "linux",
		Arch:     "amd64",
		Tags:     []string{"docker"},
		Capacity: 4,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatalf("empty session_id")
	}
	if resp.HeartbeatSeconds != heartbeatSecs {
		t.Fatalf("heartbeat = %d, want %d", resp.HeartbeatSeconds, heartbeatSecs)
	}

	s := store.New(pool)
	a, err := s.FindAgentByName(context.Background(), "runner-01")
	if err != nil {
		t.Fatalf("lookup agent: %v", err)
	}
	if a.Status != "online" {
		t.Fatalf("status = %s, want online", a.Status)
	}
	if a.Version != "0.1.0" || a.OS != "linux" || a.Arch != "amd64" || a.Capacity != 4 {
		t.Fatalf("metadata not persisted: %+v", a)
	}
	if time.Since(a.LastSeenAt) > 5*time.Second {
		t.Fatalf("last_seen_at not refreshed: %v", a.LastSeenAt)
	}
}

func TestRegister_AutoRegister_CreatesRowOnFirstHit(t *testing.T) {
	pool, client := bootServerWithAutoRegister(t, "shared-token")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId:  "agent-0",
		Token:    "shared-token",
		Tags:     []string{"linux"},
		Capacity: 2,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatalf("empty session_id")
	}

	// Row must exist post-Register, with the token hashed in place
	// so subsequent registers validate against it instead of the
	// shared registration token.
	s := store.New(pool)
	a, err := s.FindAgentByName(context.Background(), "agent-0")
	if err != nil {
		t.Fatalf("auto-registered row missing: %v", err)
	}
	if !store.VerifyToken("shared-token", a.TokenHash) {
		t.Fatalf("token hash not stored or wrong")
	}
	if a.Status != "online" {
		t.Fatalf("status = %s, want online", a.Status)
	}
}

func TestRegister_AutoRegister_RefusesWrongToken(t *testing.T) {
	_, client := bootServerWithAutoRegister(t, "shared-token")

	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "agent-0",
		Token:   "different-token",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound (auto-register must not accept wrong token)", code)
	}
}

func TestRegister_AutoRegister_OffByDefault(t *testing.T) {
	// bootServer (no auto-register token) — same as before, unknown
	// agent must be rejected with NotFound regardless of token.
	_, client := bootServer(t)
	_, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "agent-0",
		Token:   "anything",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", code)
	}
}

// TestRegister_ReclaimsOrphanedRunningJobs is the integration cover for
// the register-fence path. When an agent re-registers (e.g. k8s pod
// restart after OOM), every job_run still marked 'running' against
// that agent_id is by definition orphaned — the process that took
// those jobs is gone. The fence inside Register reclaims them BEFORE
// the new session is created so the scheduler sees the requeued
// work on its next NOTIFY tick.
//
// Without the fence, MarkAgentOnline refreshes last_seen_at=NOW so
// the reaper's INNER-JOIN-with-staleness path skips the orphans
// forever, leaving them as a permanent block on serial pipelines.
// This test asserts the end-to-end recovery: pre-state has a
// running job, Register is called, post-state shows the job
// re-queued with attempt bumped + log lines cleared.
func TestRegister_ReclaimsOrphanedRunningJobs(t *testing.T) {
	pool, client := bootServer(t)
	s := store.New(pool)
	ctx := context.Background()

	seedAgentViaSQL(t, pool, "runner-fence", store.HashToken("tok"))

	// Seed a job_run that simulates the previous process's in-flight
	// work: status='running', agent_id=<this agent>, with a log line
	// that should get cleared on reclaim.
	jobID, runID := seedOrphanedRunningJobForAgent(t, pool, "runner-fence")
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: jobID, Seq: 1, Stream: "stdout",
		Text: "from previous incarnation", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	resp, err := client.Register(ctx, &gocdnextv1.RegisterRequest{
		AgentId: "runner-fence",
		Token:   "tok",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatalf("empty session id")
	}

	var status string
	var attempt int32
	var agent *string
	if err := pool.QueryRow(ctx,
		`SELECT status, attempt, agent_id::text FROM job_runs WHERE id=$1`, jobID,
	).Scan(&status, &attempt, &agent); err != nil {
		t.Fatalf("post-register lookup: %v", err)
	}
	if status != "queued" {
		t.Fatalf("post-fence job status = %q, want queued", status)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (bumped from 0)", attempt)
	}
	if agent != nil {
		t.Fatalf("agent_id = %v, want nil (fence cleared)", agent)
	}

	var logCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM log_lines WHERE job_run_id=$1`, jobID).Scan(&logCount)
	if logCount != 0 {
		t.Fatalf("log lines remaining = %d, want 0 (cleared by reclaim)", logCount)
	}

	// The run itself stays 'running' — the scheduler will re-pick
	// the queued job and the agent will (eventually) finish it.
	var runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if runStatus == "failed" || runStatus == "canceled" || runStatus == "success" {
		t.Fatalf("run prematurely terminal: %q", runStatus)
	}
}

// TestRegister_FenceNoopOnFreshAgent — every register hits the fence.
// An agent that's never had a job MUST get a zero-cost noop, no
// errors, no log noise beyond the normal "agent registered" line.
func TestRegister_FenceNoopOnFreshAgent(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-fresh", store.HashToken("tok"))

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-fresh",
		Token:   "tok",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatalf("empty session id on fresh-agent register")
	}

	// Sanity check: no rows accidentally created or touched.
	var jobCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM job_runs`).Scan(&jobCount)
	if jobCount != 0 {
		t.Fatalf("fence created %d phantom jobs, want 0", jobCount)
	}
}

// TestSession_DecRunningTargetsSpecificSession — MED #1: the
// result handler must DecRunning on the session that actually
// accepted the assignment, not on whatever session SessionStore
// currently has for the agent. A successor-register race would
// otherwise drive the SUCCESSOR session's counter negative,
// admitting one extra concurrent dispatch beyond its declared
// capacity.
func TestSession_DecRunningTargetsSpecificSession(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := store.CreateSession(agentID, nil, 1, 0)

	jobID := uuid.New()
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_Assign{
			Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
		},
	}
	if err := store.DispatchAssignment(agentID, msg, jobID, 0); err != nil {
		t.Fatalf("DispatchAssignment: %v", err)
	}
	<-sess.Out() // drain the assign frame

	// Sanity: sess.running is now 1 (the assign bumped it). A
	// successor register swaps the SessionStore session out.
	_ = store.CreateSession(agentID, nil, 1, 0)

	// Result handler decrements DIRECTLY on the original sess.
	// If we'd instead gone via SessionStore.Release(agentID), the
	// new session would lose a running slot.
	sess.DecRunning()

	// The old session's counter is back to 0; the new session's
	// counter is still 0 (never had an assignment). Both healthy.
	// The bug would be: new session at -1, allowing one extra
	// dispatch. We assert by re-finding the agent as idle (running
	// < capacity).
	if got, ok := store.FindIdle(); !ok || got != agentID {
		t.Fatalf("FindIdle didn't return our agent: got=%v ok=%v", got, ok)
	}
}

// TestSessionStore_DispatchAssignment_IsAtomic locks in the
// combined record+enqueue contract: if a successor Register races
// between record-step and dispatch-step, the new session must NOT
// observe an Assign frame without the matching assignment entry.
// Pre-fix this was two RPC calls (RecordAssignmentForAgent +
// Dispatch); the new DispatchAssignment does both under one mutex
// hold on whichever session is current at the moment.
//
// We exercise the invariant by inspecting the session's assignment
// map after dispatch: the entry MUST live on the same session that
// received the message.
func TestSessionStore_DispatchAssignment_IsAtomic(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := store.CreateSession(agentID, nil, 1, 0)

	jobID := uuid.New()
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_Assign{
			Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
		},
	}
	if err := store.DispatchAssignment(agentID, msg, jobID, 5); err != nil {
		t.Fatalf("DispatchAssignment: %v", err)
	}

	// Drain the channel so the assertion below can compare the
	// session that received the frame against the session whose
	// assignment map carries the entry.
	select {
	case got := <-sess.Out():
		if got.GetAssign() == nil || got.GetAssign().GetJobId() != jobID.String() {
			t.Fatalf("dispatched msg mismatch: %+v", got)
		}
	default:
		t.Fatal("no message enqueued on session.Out")
	}

	attempt, ok := sess.LookupAssignment(jobID)
	if !ok {
		t.Fatal("assignment not recorded on dispatched session")
	}
	if attempt != 5 {
		t.Fatalf("attempt = %d, want 5", attempt)
	}
}

// TestSessionStore_DispatchAssignment_ClearsOnBusy covers the
// rollback path: when the session's out channel is full, the
// assignment must NOT be left behind so a scheduler retry can
// re-stamp cleanly without a phantom entry confusing the eventual
// result handler.
func TestSessionStore_DispatchAssignment_ClearsOnBusy(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := store.CreateSession(agentID, nil, 1, 0)

	// Saturate the channel: defaultSendBuffer=16; queue 16
	// non-Assign frames so any subsequent DispatchAssignment hits
	// the default-case "busy" path.
	for i := 0; i < 16; i++ {
		filler := &gocdnextv1.ServerMessage{
			Kind: &gocdnextv1.ServerMessage_Pong{Pong: &gocdnextv1.Pong{}},
		}
		if err := store.Dispatch(agentID, filler); err != nil {
			t.Fatalf("seed dispatch %d: %v", i, err)
		}
	}

	jobID := uuid.New()
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_Assign{
			Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
		},
	}
	err := store.DispatchAssignment(agentID, msg, jobID, 1)
	if err == nil {
		t.Fatal("expected ErrSessionBusy")
	}

	// Rollback check: assignment must NOT linger.
	if _, ok := sess.LookupAssignment(jobID); ok {
		t.Fatal("assignment left behind after busy dispatch — would phantom-match a future result")
	}
}

// TestSessionStore_DispatchAssignment_RejectsAttemptOverwrite is the
// secondary trip-wire for the reaper-without-fence HIGH race.
// Scenario: an agent's session is stale (heartbeat lost), the reaper
// requeues its job (attempt N → N+1), but a notify fires before the
// session is revoked. The scheduler wakes up, FindIdle still sees
// the stale session (it has spare capacity in memory now that its
// job was reclaimed out from under it), and DispatchAssignment
// re-targets the same session with attempt N+1.
//
// Without CAS, RecordAssignment would silently overwrite N→N+1; a
// late JobResult from the OLD attempt would look up "N+1", match
// the new row's snapshot CAS, and complete the new attempt with
// the old payload. The CAS gate REFUSES the overwrite — caller
// gets ErrSessionBusy, scheduler picks a different agent (or
// leaves the job queued for the next tick once the fence kills
// this session).
func TestSessionStore_DispatchAssignment_RejectsAttemptOverwrite(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := store.CreateSession(agentID, nil, 2, 0)

	jobID := uuid.New()
	msg := func(attempt int32) *gocdnextv1.ServerMessage {
		return &gocdnextv1.ServerMessage{
			Kind: &gocdnextv1.ServerMessage_Assign{
				Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
			},
		}
	}

	// First dispatch: attempt=1 succeeds.
	if err := store.DispatchAssignment(agentID, msg(1), jobID, 1); err != nil {
		t.Fatalf("first DispatchAssignment: %v", err)
	}
	<-sess.Out() // drain

	// Second dispatch for the SAME jobID with a DIFFERENT attempt
	// must be refused as busy. (Same attempt would be idempotent
	// and is covered separately below.)
	err := store.DispatchAssignment(agentID, msg(2), jobID, 2)
	if !errors.Is(err, grpcsrv.ErrSessionBusy) {
		t.Fatalf("attempt-overwrite err = %v, want ErrSessionBusy", err)
	}

	// The stale recorded attempt must remain intact — the result
	// handler still needs it to validate against incoming JobResult
	// frames from the original attempt's in-flight work.
	got, ok := sess.LookupAssignment(jobID)
	if !ok {
		t.Fatal("original assignment dropped by failed overwrite")
	}
	if got != 1 {
		t.Fatalf("assignment overwritten anyway: got %d, want 1", got)
	}

	// And the second Assign frame MUST NOT have been delivered —
	// the agent should never see the new attempt while still owning
	// the old one.
	select {
	case extra := <-sess.Out():
		t.Fatalf("attempt-overwrite Assign leaked onto session.Out: %+v", extra)
	default:
	}
}

// TestSessionStore_DispatchAssignment_IdempotentSameAttempt is the
// companion: the CAS must accept a re-dispatch of the SAME
// (jobID, attempt) so a scheduler retry that re-arrives at the
// same plan after a transient failure isn't blocked spuriously.
func TestSessionStore_DispatchAssignment_IdempotentSameAttempt(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := store.CreateSession(agentID, nil, 2, 0)

	jobID := uuid.New()
	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_Assign{
			Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
		},
	}
	if err := store.DispatchAssignment(agentID, msg, jobID, 7); err != nil {
		t.Fatalf("first DispatchAssignment: %v", err)
	}
	<-sess.Out()
	if err := store.DispatchAssignment(agentID, msg, jobID, 7); err != nil {
		t.Fatalf("idempotent re-dispatch should succeed, got: %v", err)
	}
	<-sess.Out()
	got, ok := sess.LookupAssignment(jobID)
	if !ok || got != 7 {
		t.Fatalf("LookupAssignment got (%d, %v), want (7, true)", got, ok)
	}
}

// TestSessionStore_SupersededFlagOnRevokeForAgent covers HIGH #2's
// race window: the OLD stream's defer can fire AFTER RevokeForAgent
// but BEFORE the successor's CreateSession publishes the new session
// in latestByAg. In that window IsAgentSuperseded returns false (no
// new latest yet), so the defer would normally MarkAgentOffline and
// clobber the agents row before MarkAgentOnline runs.
//
// supersededByRegister, set inside RevokeForAgent, gives the defer
// a definitive signal even before CreateSession lands. This test
// drives the SessionStore primitive directly so the regression
// stays cheap to maintain.
func TestSessionStore_SupersededFlagOnRevokeForAgent(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()

	// Old session in place.
	old := store.CreateSession(agentID, nil, 1, 0)
	if old.SupersededByRegisterForTest() {
		t.Fatal("fresh session reported superseded")
	}

	// RevokeForAgent fires (simulates the new Register's revoke
	// step). The OLD session should now report superseded=true
	// so its defer skips MarkAgentOffline — even though
	// CreateSession for the successor hasn't run yet.
	store.RevokeForAgent(agentID)
	if !old.SupersededByRegisterForTest() {
		t.Fatal("RevokeForAgent didn't set supersededByRegister; defer would clobber online row")
	}
}

// TestSessionStore_SupersededFlagOnCreateSessionInternalRevoke locks
// in the second path that flips the flag: when CreateSession itself
// revokes the prior session (without an explicit RevokeForAgent call).
// Belt-and-suspenders coverage of the dual flag-setting strategy.
func TestSessionStore_SupersededFlagOnCreateSessionInternalRevoke(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentID := uuid.New()

	old := store.CreateSession(agentID, nil, 1, 0)
	// New session via CreateSession (not via RevokeForAgent first).
	_ = store.CreateSession(agentID, nil, 1, 0)
	if !old.SupersededByRegisterForTest() {
		t.Fatal("CreateSession's internal revoke didn't set supersededByRegister")
	}
}

// TestSessionStore_IsAgentSupersededGuardsOfflineMark drives the
// HIGH #2 race the reviewer caught: an OLD agent stream's defer
// MUST NOT MarkAgentOffline after a SUCCESSOR session has Registered
// — otherwise the reaper sees agent.status='offline' (which it
// treats as always-stale regardless of last_seen_at) and reclaims
// the new session's healthy jobs.
//
// The Connect handler's defer uses sessions.IsAgentSuperseded to
// decide whether to flip agents.status. This test exercises the
// SessionStore predicate directly across the three relevant cases:
// no successor (normal disconnect), successor present (suppress
// offline), and missing session (idempotent no-op).
func TestSessionStore_IsAgentSupersededGuardsOfflineMark(t *testing.T) {
	store := grpcsrv.NewSessionStore()
	agentA := uuid.New()
	agentB := uuid.New()

	// Case 1: agent connects, closes, no successor. Latest entry is
	// gone after Revoke (it WAS the latest, so Revoke deletes it).
	// IsAgentSuperseded should return false → defer marks offline.
	sess := store.CreateSession(agentA, nil, 1, 0)
	store.Revoke(sess.ID)
	if store.IsAgentSuperseded(agentA, sess.ID) {
		t.Fatalf("normal disconnect reported as superseded (would skip MarkAgentOffline)")
	}

	// Case 2: agent connects, a SUCCESSOR session takes over while
	// the old stream is still draining. The old stream's defer runs
	// here; IsAgentSuperseded should return TRUE so the defer
	// suppresses MarkAgentOffline.
	old := store.CreateSession(agentB, nil, 1, 0)
	newer := store.CreateSession(agentB, nil, 1, 0) // supersedes — revokes old internally
	if !store.IsAgentSuperseded(agentB, old.ID) {
		t.Fatalf("superseded-old reported as NOT superseded — would clobber agent online")
	}
	// Closing the NEW one (which IS the latest) reports false: a
	// stream closing its OWN current session is the normal terminal
	// disconnect, defer should mark offline.
	if store.IsAgentSuperseded(agentB, newer.ID) {
		t.Fatalf("current session reported as superseded by itself")
	}

	// Case 3: completely unknown agent → false (no session, normal
	// disconnect path runs MarkAgentOffline which is a no-op SQL
	// when the row is already offline).
	ghost := uuid.New()
	if store.IsAgentSuperseded(ghost, "any-session-id") {
		t.Fatalf("ghost agent reported as superseded")
	}
}

func TestRegister_InvalidArgs(t *testing.T) {
	_, client := bootServer(t)

	tests := []struct {
		name string
		req  *gocdnextv1.RegisterRequest
	}{
		{"missing agent_id", &gocdnextv1.RegisterRequest{Token: "t"}},
		{"missing token", &gocdnextv1.RegisterRequest{AgentId: "a"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.Register(context.Background(), tt.req)
			if code := status.Code(err); code != codes.InvalidArgument {
				t.Fatalf("code = %s, want InvalidArgument", code)
			}
		})
	}
}

// --- test harness ---

func bootServer(t *testing.T) (*pgxpool.Pool, gocdnextv1.AgentServiceClient) {
	return bootServerWithAutoRegister(t, "")
}

func bootServerWithAutoRegister(t *testing.T, autoRegToken string) (*pgxpool.Pool, gocdnextv1.AgentServiceClient) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	svc := grpcsrv.NewAgentService(s, grpcsrv.NewSessionStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeatSecs).
		WithAutoRegisterToken(autoRegToken)

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(grpcSrv, svc)
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
		_ = lis.Close()
	})
	return pool, gocdnextv1.NewAgentServiceClient(conn)
}

// seedOrphanedRunningJobForAgent inserts the minimal project →
// pipeline → run → stage_run → job_run chain needed to simulate the
// "previous agent process left a running job behind" scenario the
// fence is designed to clean up.
//
// Inserts raw SQL instead of going through ApplyProject /
// CreateRunFromModification because the test only needs the row
// END-STATE — full lifecycle helpers in store_test aren't exported
// across packages anyway. Returns the job_run id + parent run id
// so the test can assert post-fence state on both.
func seedOrphanedRunningJobForAgent(t *testing.T, pool *pgxpool.Pool, agentName string) (jobID, runID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var agentID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM agents WHERE name=$1`, agentName).Scan(&agentID); err != nil {
		t.Fatalf("orphan seed: lookup agent: %v", err)
	}

	var projectID, pipelineID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (slug, name) VALUES ($1, 'fence-test') RETURNING id`,
		"fence-test-"+agentName,
	).Scan(&projectID); err != nil {
		t.Fatalf("orphan seed: project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO pipelines (project_id, name, definition) VALUES ($1, 'p', '{}'::jsonb) RETURNING id`,
		projectID,
	).Scan(&pipelineID); err != nil {
		t.Fatalf("orphan seed: pipeline: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (pipeline_id, counter, cause, status, revisions, started_at)
		 VALUES ($1, 1, 'manual', 'running', '{}'::jsonb, NOW())
		 RETURNING id`,
		pipelineID,
	).Scan(&runID); err != nil {
		t.Fatalf("orphan seed: run: %v", err)
	}
	var stageRunID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO stage_runs (run_id, name, ordinal, status, started_at)
		 VALUES ($1, 'build', 0, 'running', NOW())
		 RETURNING id`,
		runID,
	).Scan(&stageRunID); err != nil {
		t.Fatalf("orphan seed: stage_run: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO job_runs (run_id, stage_run_id, name, agent_id, status, started_at)
		 VALUES ($1, $2, 'compile', $3, 'running', NOW())
		 RETURNING id`,
		runID, stageRunID, agentID,
	).Scan(&jobID); err != nil {
		t.Fatalf("orphan seed: job_run: %v", err)
	}
	return
}

func seedAgentViaSQL(t *testing.T, pool *pgxpool.Pool, name, tokenHash string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, $2)`,
		name, tokenHash,
	)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}
