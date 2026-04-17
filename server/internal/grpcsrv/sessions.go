// Package grpcsrv implements the gRPC services exposed by the control-plane:
// AgentService (Register + Connect bidi) today, more endpoints later.
package grpcsrv

import (
	"sync"

	"github.com/google/uuid"
)

// SessionStore keeps the mapping session_id → agent_id in-memory. Sessions are
// ephemeral: they exist only while an agent is connected. For multi-server HA
// this would move to Redis, but single-process is enough for the MVP.
type SessionStore struct {
	mu         sync.Mutex
	byID       map[string]uuid.UUID
	latestByAg map[uuid.UUID]string
}

// NewSessionStore returns an empty, ready-to-use store.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		byID:       make(map[string]uuid.UUID),
		latestByAg: make(map[uuid.UUID]string),
	}
}

// Create issues a new session id for the given agent and invalidates any
// previous session the agent had — a re-registration supersedes a zombie
// stream that might still be reading with stale credentials.
func (s *SessionStore) Create(agentID uuid.UUID) string {
	sess := uuid.NewString()

	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.latestByAg[agentID]; ok {
		delete(s.byID, prev)
	}
	s.byID[sess] = agentID
	s.latestByAg[agentID] = sess
	return sess
}

// Lookup returns the agent id for the session and whether it is still valid.
func (s *SessionStore) Lookup(session string) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byID[session]
	return id, ok
}

// Revoke drops the session. Safe to call on missing/unknown ids.
func (s *SessionStore) Revoke(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agentID, ok := s.byID[session]
	if !ok {
		return
	}
	delete(s.byID, session)
	if s.latestByAg[agentID] == session {
		delete(s.latestByAg, agentID)
	}
}
