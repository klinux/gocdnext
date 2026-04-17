package grpcsrv_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
)

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
	if got != agentID {
		t.Fatalf("Lookup got %s, want %s", got, agentID)
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
	if got, ok := s.Lookup(second); !ok || got != agentID {
		t.Fatalf("second session lost: ok=%v got=%s want=%s", ok, got, agentID)
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
				if got, ok := s.Lookup(sess); !ok || got != id {
					t.Errorf("lost session under concurrency")
					return
				}
				s.Revoke(sess)
			}
		}()
	}
	wg.Wait()
}
