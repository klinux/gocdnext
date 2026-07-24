package grpcsrv_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
)

func noopMsg() *gocdnextv1.ServerMessage {
	return &gocdnextv1.ServerMessage{Kind: &gocdnextv1.ServerMessage_Pong{Pong: &gocdnextv1.Pong{}}}
}

func TestSessionStore_CreateReturnsDistinctIDs(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()

	a := s.Create(agentID)
	b := s.Create(agentID)
	if a == b {
		t.Fatalf("consecutive sessions collided: %s", a)
	}
	if a == "" || b == "" {
		t.Fatalf("empty session id")
	}
}

func TestSessionStore_LookupReturnsAgent(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.Create(agentID)

	got, ok := s.Lookup(sess)
	if !ok {
		t.Fatalf("Lookup ok=false, want true")
	}
	if got.AgentID != agentID {
		t.Fatalf("Lookup got %s, want %s", got.AgentID, agentID)
	}
}

func TestSessionStore_LookupMissing(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	_, ok := s.Lookup("unknown-session")
	if ok {
		t.Fatalf("Lookup ok=true on unknown session")
	}
}

func TestSessionStore_Revoke(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	sess := s.Create(uuid.New())
	s.Revoke(sess)

	if _, ok := s.Lookup(sess); ok {
		t.Fatalf("revoked session still resolves")
	}
}

func TestSessionStore_CreateReplacesPrevious(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()

	first := s.Create(agentID)
	second := s.Create(agentID)

	// Second Register from the same agent supersedes the first: old session
	// becomes invalid so a zombie stream cannot linger with stale auth.
	if _, ok := s.Lookup(first); ok {
		t.Fatalf("first session still valid after second Create")
	}
	got, ok := s.Lookup(second)
	if !ok || got.AgentID != agentID {
		t.Fatalf("second session lost: ok=%v agent=%s want=%s", ok, got.AgentID, agentID)
	}
}

func TestSessionStore_DispatchDeliversToCurrentSession(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.CreateSession(agentID, nil, 2, 0)

	msg := noopMsg()
	if err := s.Dispatch(agentID, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got := <-sess.Out()
	if got != msg {
		t.Fatalf("received different message")
	}
}

func TestSessionStore_DispatchNoSession(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	err := s.Dispatch(uuid.New(), noopMsg())
	if !errors.Is(err, grpcsrv.ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

func TestSessionStore_DispatchBusyWhenQueueFull(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	_ = s.CreateSession(agentID, nil, 1, 0)

	// defaultSendBuffer is 16; fill it with messages and the next Dispatch
	// should fail fast with ErrSessionBusy rather than block the caller.
	for i := 0; i < 16; i++ {
		if err := s.Dispatch(agentID, noopMsg()); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if err := s.Dispatch(agentID, noopMsg()); !errors.Is(err, grpcsrv.ErrSessionBusy) {
		t.Fatalf("err = %v, want ErrSessionBusy", err)
	}
}

func TestSessionStore_FindIdlePicksConnectedAgent(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	want := uuid.New()
	sess := s.CreateSession(want, nil, 1, 0)
	s.MarkReadyForTest(sess.ID)

	got, ok := s.FindIdle()
	if !ok || got != want {
		t.Fatalf("FindIdle got (%s, %v), want (%s, true)", got, ok, want)
	}
}

func TestSessionStore_FindIdleSkipsUnreadySession(t *testing.T) {
	t.Parallel()

	// A session published at Register but not yet Connect-ready is NOT
	// schedulable — the pre-existing latent-bug fix (#178).
	s := grpcsrv.NewSessionStore()
	sess := s.CreateSession(uuid.New(), nil, 1, 0)
	if _, ok := s.FindIdle(); ok {
		t.Fatal("FindIdle selected a not-yet-ready session")
	}
	s.MarkReadyForTest(sess.ID)
	if _, ok := s.FindIdle(); !ok {
		t.Fatal("FindIdle did not select a ready session")
	}
}

func TestSessionStore_FindIdleRespectsCapacity(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.CreateSession(agentID, nil, 1, 0)
	s.MarkReadyForTest(sess.ID)
	sess.IncRunning()

	if _, ok := s.FindIdle(); ok {
		t.Fatalf("FindIdle returned a session at full capacity")
	}
	sess.DecRunning()
	if _, ok := s.FindIdle(); !ok {
		t.Fatalf("FindIdle did not see freed capacity")
	}
}

func TestSessionStore_FindIdleWithTags_MatchesSupersets(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	linuxAgent := uuid.New()
	dockerAgent := uuid.New()
	ls := s.CreateSession(linuxAgent, []string{"linux"}, 1, 0)
	ds := s.CreateSession(dockerAgent, []string{"linux", "docker"}, 1, 0)
	s.MarkReadyForTest(ls.ID)
	s.MarkReadyForTest(ds.ID)

	// Requiring just "linux" can hit either agent.
	if _, ok := s.FindIdleWithTags([]string{"linux"}); !ok {
		t.Fatalf("expected a match for required=[linux]")
	}

	// Requiring "docker" must pick dockerAgent.
	got, ok := s.FindIdleWithTags([]string{"docker"})
	if !ok || got != dockerAgent {
		t.Fatalf("required=[docker] got (%s, %v), want (%s, true)", got, ok, dockerAgent)
	}

	// Requiring "gpu" (no agent has it) must fail.
	if _, ok := s.FindIdleWithTags([]string{"gpu"}); ok {
		t.Fatalf("expected no match for required=[gpu]")
	}

	// Empty required list matches any agent (same as FindIdle).
	if _, ok := s.FindIdleWithTags(nil); !ok {
		t.Fatalf("empty required should match any agent")
	}
}

func TestSessionStore_FindIdleWithTags_RespectsCapacityAndRevoked(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agent := uuid.New()
	sess := s.CreateSession(agent, []string{"linux", "docker"}, 1, 0)
	s.MarkReadyForTest(sess.ID)
	sess.IncRunning()
	if _, ok := s.FindIdleWithTags([]string{"docker"}); ok {
		t.Fatalf("should not match when capacity exhausted")
	}
	sess.DecRunning()
	if _, ok := s.FindIdleWithTags([]string{"docker"}); !ok {
		t.Fatalf("should match after capacity frees")
	}

	s.Revoke(sess.ID)
	if _, ok := s.FindIdleWithTags([]string{"docker"}); ok {
		t.Fatalf("revoked session should not match")
	}
}

func TestSessionStore_RevokeClosesChannel(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	sess := s.CreateSession(uuid.New(), nil, 1, 0)
	s.Revoke(sess.ID)

	if _, ok := <-sess.Out(); ok {
		t.Fatalf("Out channel still open after Revoke")
	}
}

func TestSessionStore_OnSessionReadyFiresAfterMarkReady(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	fired := make(chan struct{}, 1)
	s.SetOnSessionReady(func() { fired <- struct{}{} })

	// onReady must NOT fire at CreateSession (no consumer yet) — only once the
	// Connect stream is ready.
	sess := s.CreateSession(uuid.New(), []string{"linux"}, 2, 0)
	select {
	case <-fired:
		t.Fatal("OnSessionReady fired at CreateSession, before Connect")
	case <-time.After(50 * time.Millisecond):
	}

	s.MarkReadyForTest(sess.ID)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("OnSessionReady didn't fire within 1s of MarkReady")
	}
}

func TestSessionStore_OnlineCountWalksLifecycle(t *testing.T) {
	t.Parallel()

	// AgentsOnline reflects READY sessions, not published ones: it stays 0
	// through CreateSession and ClaimConnect, becomes 1 at MarkReady, and drops
	// back to 0 on Revoke.
	s := grpcsrv.NewSessionStore()
	sess := s.CreateSession(uuid.New(), nil, 1, 0)
	if got := s.ReadyCountForTest(); got != 0 {
		t.Fatalf("after CreateSession: online=%d, want 0", got)
	}
	if _, err := s.ClaimConnect(sess.ID); err != nil {
		t.Fatalf("ClaimConnect: %v", err)
	}
	if got := s.ReadyCountForTest(); got != 0 {
		t.Fatalf("after ClaimConnect: online=%d, want 0", got)
	}
	if err := s.MarkReady(sess.ID); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if got := s.ReadyCountForTest(); got != 1 {
		t.Fatalf("after MarkReady: online=%d, want 1", got)
	}
	s.Revoke(sess.ID)
	if got := s.ReadyCountForTest(); got != 0 {
		t.Fatalf("after Revoke: online=%d, want 0", got)
	}
}

func TestSessionStore_DrainingSkipsSelectionButStaysLive(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.CreateSession(agentID, nil, 1, 0)
	s.MarkReadyForTest(sess.ID)

	// Selectable while live.
	if _, ok := s.FindIdleWithTags(nil); !ok {
		t.Fatal("ready session should be selectable")
	}

	sess.SetDrainingForTest()

	// Not selected, and dispatch is refused with ErrSessionDraining.
	if _, ok := s.FindIdleWithTags(nil); ok {
		t.Fatal("draining session must not be selected")
	}
	jobID := uuid.New()
	assign := &gocdnextv1.ServerMessage{Kind: &gocdnextv1.ServerMessage_Assign{
		Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
	}}
	if err := s.DispatchAssignment(agentID, assign, jobID, 0); !errors.Is(err, grpcsrv.ErrSessionDraining) {
		t.Fatalf("DispatchAssignment on draining = %v, want ErrSessionDraining", err)
	}
	// But it stays LIVE: Dispatch (Pong/Cancel/Cleanup) still succeeds.
	if err := s.Dispatch(agentID, noopMsg()); err != nil {
		t.Fatalf("Dispatch (pong) to draining session failed: %v", err)
	}
}

func TestSessionStore_DuplicateDispatchDoesNotDoubleCount(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.CreateSession(agentID, nil, 2, 0)
	s.MarkReadyForTest(sess.ID)

	jobID := uuid.New()
	assign := &gocdnextv1.ServerMessage{Kind: &gocdnextv1.ServerMessage_Assign{
		Assign: &gocdnextv1.JobAssignment{JobId: jobID.String()},
	}}
	// First dispatch: running=1.
	if err := s.DispatchAssignment(agentID, assign, jobID, 0); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	<-sess.Out()
	if got := sess.Running(); got != 1 {
		t.Fatalf("after first dispatch running=%d, want 1", got)
	}
	// Duplicate dispatch of the SAME (job, attempt): idempotent no-op — running
	// must stay 1, and no second frame is enqueued.
	if err := s.DispatchAssignment(agentID, assign, jobID, 0); err != nil {
		t.Fatalf("duplicate dispatch returned error: %v", err)
	}
	if got := sess.Running(); got != 1 {
		t.Fatalf("after duplicate dispatch running=%d, want 1 (counter leaked)", got)
	}
	select {
	case m := <-sess.Out():
		t.Fatalf("duplicate dispatch enqueued a second frame: %v", m)
	default:
	}
}

func TestSessionStore_SecondConnectRejected(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	sess := s.CreateSession(uuid.New(), nil, 1, 0)
	if _, err := s.ClaimConnect(sess.ID); err != nil {
		t.Fatalf("first ClaimConnect: %v", err)
	}
	if _, err := s.ClaimConnect(sess.ID); err == nil {
		t.Fatal("second ClaimConnect on the same session should be rejected")
	}
}

func TestSessionStore_OnSessionReadyNilSafe(t *testing.T) {
	t.Parallel()

	// No callback registered — CreateSession must not panic or
	// block. Regression guard for the nil-hook path on boot,
	// before the scheduler has wired itself in.
	s := grpcsrv.NewSessionStore()
	_ = s.CreateSession(uuid.New(), nil, 1, 0)
}

// TestSessionStore_FenceStaleSession_RevokesMatchingGeneration —
// happy path: the reaper observed generation N at SELECT, the live
// session still carries N, the fence revokes it.
func TestSessionStore_FenceStaleSession_RevokesMatchingGeneration(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	// Pass generation in the constructor — sess.generation is
	// immutable after CreateSession returns (round 12 fix).
	sess := s.CreateSession(agentID, nil, 1, 5)

	if got := s.FenceStaleSession(agentID, 5); got != grpcsrv.FenceResultRevoked {
		t.Fatalf("FenceStaleSession = %v, want FenceResultRevoked", got)
	}
	if _, ok := s.Lookup(sess.ID); ok {
		t.Fatal("session still resolves after fence")
	}
	// Out channel must be closed so the Connect pump exits.
	if _, ok := <-sess.Out(); ok {
		t.Fatal("out channel still open after fence")
	}
}

// TestSessionStore_FenceStaleSession_SkipsGenerationMismatch is the
// round-11 HIGH regression test at the SessionStore layer. The
// reaper snapshotted generation 5 from the agents table, but
// between then and now the agent re-Registered, bumping the live
// session's Generation to 6. FenceStaleSession MUST NOT revoke
// the successor.
func TestSessionStore_FenceStaleSession_SkipsGenerationMismatch(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	// Successor session published with generation=6 (already bumped
	// by MarkAgentOnline). Reaper's observed snapshot is 5.
	sess := s.CreateSession(agentID, nil, 1, 6)

	if got := s.FenceStaleSession(agentID, 5); got != grpcsrv.FenceResultGenerationChanged {
		t.Fatalf("FenceStaleSession = %v, want FenceResultGenerationChanged (must not revoke successor)", got)
	}
	if _, ok := s.Lookup(sess.ID); !ok {
		t.Fatal("successor session disappeared despite generation mismatch")
	}
	// Out channel must still be open — the pump should keep running.
	select {
	case _, ok := <-sess.Out():
		if !ok {
			t.Fatal("out channel closed despite generation mismatch")
		}
	default:
		// expected: no message yet, channel open
	}
}

// TestSessionStore_FenceStaleSession_NoSessionForAgent — calling
// fence on an agent that has no live session is a no-op, not a
// panic. Mirrors the production case where the agent's stream
// already EOF'd before the reaper got there.
func TestSessionStore_FenceStaleSession_NoSessionForAgent(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	if got := s.FenceStaleSession(uuid.New(), 1); got != grpcsrv.FenceResultNoSession {
		t.Fatalf("FenceStaleSession = %v, want FenceResultNoSession", got)
	}
}

// TestSessionStore_FenceStaleSession_DoesNotMarkSuperseded covers
// the round-11 MED: RevokeForAgent sets supersededByRegister=true
// so the Connect handler's defer skips MarkAgentOffline. The reaper
// path WANTS the offline mark (the agent really is dead); fence
// must not set the flag.
func TestSessionStore_FenceStaleSession_DoesNotMarkSuperseded(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.CreateSession(agentID, nil, 1, 2)

	if got := s.FenceStaleSession(agentID, 2); got != grpcsrv.FenceResultRevoked {
		t.Fatalf("FenceStaleSession = %v, want FenceResultRevoked", got)
	}
	// Even though the session is revoked, the superseded-by-register
	// flag must remain false — the Connect defer reads this to
	// decide whether to MarkAgentOffline. Reaper path: yes please.
	if sess.SupersededByRegisterForTest() {
		t.Fatal("supersededByRegister was set by FenceStaleSession — would block the offline mark in the Connect defer")
	}
	if !sess.RevokedForTest() {
		t.Fatal("session revoked flag should be true after fence")
	}
}

// TestSessionStore_AllAgentIDs_EngineFilter — the cleanup-broadcast
// target set is filtered to k8s + legacy-empty engines at the
// in-memory layer (mirrors the SQL filter in ListAgentsForRun).
// Without this in-mem filter, a docker session connected now would
// receive cleanups for runs whose k8s agent went offline → no-op
// success masks the leak.
func TestSessionStore_AllAgentIDs_EngineFilter(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	k8sID := uuid.New()
	dockerID := uuid.New()
	legacyID := uuid.New() // empty Engine string

	s.CreateSession(k8sID, nil, 1, 1, grpcsrv.CreateSessionOpts{Engine: "kubernetes"})
	s.CreateSession(dockerID, nil, 1, 1, grpcsrv.CreateSessionOpts{Engine: "docker"})
	s.CreateSession(legacyID, nil, 1, 1) // no engine opts → ""

	// No filter → every connected agent.
	all := s.AllAgentIDs("")
	if len(all) != 3 {
		t.Fatalf("AllAgentIDs(\"\") len = %d, want 3 (no filter): %v", len(all), all)
	}

	// k8s filter → only k8s + legacy (defensive).
	k8s := s.AllAgentIDs("kubernetes")
	seen := map[uuid.UUID]bool{}
	for _, id := range k8s {
		seen[id] = true
	}
	if !seen[k8sID] {
		t.Errorf("k8s agent missing from k8s filter: got %v", k8s)
	}
	if !seen[legacyID] {
		t.Errorf("legacy (engine='') agent missing from k8s filter: got %v", k8s)
	}
	if seen[dockerID] {
		t.Errorf("docker agent leaked through k8s filter: got %v", k8s)
	}
}

func TestSessionStore_ConcurrentUse(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	const goroutines = 32
	const perG = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				id := uuid.New()
				sess := s.Create(id)
				if got, ok := s.Lookup(sess); !ok || got.AgentID != id {
					t.Errorf("lost session under concurrency")
					return
				}
				s.Revoke(sess)
			}
		}()
	}
	wg.Wait()
}
