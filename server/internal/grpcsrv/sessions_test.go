package grpcsrv_test

import (
	"errors"
	"sync"
	"testing"

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
	sess := s.CreateSession(agentID, nil, 2)

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
	_ = s.CreateSession(agentID, nil, 1)

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
	_ = s.CreateSession(want, nil, 1)

	got, ok := s.FindIdle()
	if !ok || got != want {
		t.Fatalf("FindIdle got (%s, %v), want (%s, true)", got, ok, want)
	}
}

func TestSessionStore_FindIdleRespectsCapacity(t *testing.T) {
	t.Parallel()

	s := grpcsrv.NewSessionStore()
	agentID := uuid.New()
	sess := s.CreateSession(agentID, nil, 1)
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
	s.CreateSession(linuxAgent, []string{"linux"}, 1)
	s.CreateSession(dockerAgent, []string{"linux", "docker"}, 1)

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
	sess := s.CreateSession(agent, []string{"linux", "docker"}, 1)
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
	sess := s.CreateSession(uuid.New(), nil, 1)
	s.Revoke(sess.ID)

	if _, ok := <-sess.Out(); ok {
		t.Fatalf("Out channel still open after Revoke")
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
