// Package grpcsrv implements the gRPC services exposed by the control-plane:
// AgentService (Register + Connect bidi) today, more endpoints later.
package grpcsrv

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
)

// Sentinel errors for dispatch attempts.
var (
	ErrNoSession      = errors.New("grpcsrv: no active session for agent")
	ErrSessionBusy    = errors.New("grpcsrv: session send queue full")
	ErrSessionRevoked = errors.New("grpcsrv: session revoked")
)

const defaultSendBuffer = 16

// Session represents one live agent stream. The Connect handler drains `Out`
// onto the gRPC stream, the scheduler writes to it via Dispatch.
type Session struct {
	ID       string
	AgentID  uuid.UUID
	Tags     []string
	Capacity int32

	out     chan *gocdnextv1.ServerMessage
	running atomic.Int32
	revoked atomic.Bool
}

// Out returns the receive side of the session's send queue. The Connect
// handler reads from this and ships each message to the agent.
func (s *Session) Out() <-chan *gocdnextv1.ServerMessage { return s.out }

// SessionStore keeps the mapping session_id → Session in-memory. Sessions are
// ephemeral: they exist only while an agent is connected. For multi-server HA
// this would move to Redis, but single-process is enough for the MVP.
type SessionStore struct {
	mu         sync.Mutex
	byID       map[string]*Session
	latestByAg map[uuid.UUID]string
	// onReady fires after a session is published — the scheduler
	// uses this to drain queued runs the moment an agent comes
	// online instead of waiting up to the next periodic tick.
	// Called in a goroutine so CreateSession stays non-blocking.
	onReady func()
}

// NewSessionStore returns an empty, ready-to-use store.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		byID:       make(map[string]*Session),
		latestByAg: make(map[uuid.UUID]string),
	}
}

// Create issues a new session for the given agent (MVP: no tags/capacity). It
// exists so older callsites and tests that only care about session identity
// keep working; richer metadata comes in via CreateSession.
func (s *SessionStore) Create(agentID uuid.UUID) string {
	return s.CreateSession(agentID, nil, 1).ID
}

// CreateSession issues a session and invalidates any previous session the
// agent had — a re-registration supersedes a zombie stream that might still
// be reading with stale credentials. The returned Session owns a fresh send
// queue that Revoke will close.
func (s *SessionStore) CreateSession(agentID uuid.UUID, tags []string, capacity int32) *Session {
	if capacity <= 0 {
		capacity = 1
	}
	sess := &Session{
		ID:       uuid.NewString(),
		AgentID:  agentID,
		Tags:     append([]string(nil), tags...),
		Capacity: capacity,
		out:      make(chan *gocdnextv1.ServerMessage, defaultSendBuffer),
	}

	s.mu.Lock()
	if prev, ok := s.latestByAg[agentID]; ok {
		if prevSess, ok := s.byID[prev]; ok {
			prevSess.revoked.Store(true)
			close(prevSess.out)
		}
		delete(s.byID, prev)
	}
	s.byID[sess.ID] = sess
	s.latestByAg[agentID] = sess.ID
	onReady := s.onReady
	gauge := float64(len(s.latestByAg))
	s.mu.Unlock()
	metrics.AgentsOnline.Set(gauge)

	// Fire the ready hook outside the lock so a slow subscriber
	// (scheduler draining the DB) can't stall agent registration.
	if onReady != nil {
		go onReady()
	}
	return sess
}

// SetOnSessionReady registers a callback invoked after every
// CreateSession. The scheduler uses it to drain the queued-run
// backlog immediately when an agent connects — without it, a run
// queued while no agents were online would wait up to one
// scheduler tick to find its newly-registered agent.
//
// Safe to call at any point; nil clears the hook.
func (s *SessionStore) SetOnSessionReady(fn func()) {
	s.mu.Lock()
	s.onReady = fn
	s.mu.Unlock()
}

// Lookup returns the Session for the given id and whether it is still valid.
func (s *SessionStore) Lookup(sessionID string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[sessionID]
	return sess, ok
}

// Revoke drops the session and closes its send queue. Safe to call on
// missing/unknown ids — calling it on the same session twice is also safe.
func (s *SessionStore) Revoke(sessionID string) {
	s.mu.Lock()
	sess, ok := s.byID[sessionID]
	if !ok {
		s.mu.Unlock()
		return
	}
	if sess.revoked.CompareAndSwap(false, true) {
		close(sess.out)
	}
	delete(s.byID, sessionID)
	if s.latestByAg[sess.AgentID] == sessionID {
		delete(s.latestByAg, sess.AgentID)
	}
	gauge := float64(len(s.latestByAg))
	s.mu.Unlock()
	metrics.AgentsOnline.Set(gauge)
}

// Dispatch enqueues msg onto the agent's current session. Returns ErrNoSession
// if the agent is not connected, ErrSessionBusy if the queue is full. Callers
// should treat busy as "try another agent".
func (s *SessionStore) Dispatch(agentID uuid.UUID, msg *gocdnextv1.ServerMessage) error {
	s.mu.Lock()
	id, ok := s.latestByAg[agentID]
	var sess *Session
	if ok {
		sess = s.byID[id]
	}
	s.mu.Unlock()

	if sess == nil || sess.revoked.Load() {
		return ErrNoSession
	}
	select {
	case sess.out <- msg:
		// Only JobAssignment messages bump the running counter — heartbeats,
		// pongs and future cancel frames don't consume capacity.
		if msg.GetAssign() != nil {
			sess.IncRunning()
		}
		return nil
	default:
		return ErrSessionBusy
	}
}

// Release decrements the running counter for the agent's current session.
// No-op if the agent has no live session (e.g., disconnected before the
// job result came in). Called by the result handler in C5.
func (s *SessionStore) Release(agentID uuid.UUID) {
	s.mu.Lock()
	id, ok := s.latestByAg[agentID]
	var sess *Session
	if ok {
		sess = s.byID[id]
	}
	s.mu.Unlock()
	if sess != nil {
		sess.DecRunning()
	}
}

// FindIdle returns one agent id whose current running-jobs counter is below
// its declared capacity. Equivalent to FindIdleWithTags(nil) — preserved for
// callers that don't care about tag matching.
func (s *SessionStore) FindIdle() (uuid.UUID, bool) {
	return s.FindIdleWithTags(nil)
}

// FindIdleWithTags returns one agent id whose tags are a superset of the
// required list AND which still has spare capacity. An empty `required` list
// matches any agent (same as FindIdle). Returns uuid.Nil, false when no
// session qualifies — the scheduler leaves the job queued for the next tick.
//
// Matching is AND: every required tag must be present. Case-sensitive.
func (s *SessionStore) FindIdleWithTags(required []string) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.latestByAg {
		sess, ok := s.byID[id]
		if !ok || sess.revoked.Load() {
			continue
		}
		if sess.running.Load() >= sess.Capacity {
			continue
		}
		if !hasAllTags(sess.Tags, required) {
			continue
		}
		return sess.AgentID, true
	}
	return uuid.Nil, false
}

func hasAllTags(have, required []string) bool {
	if len(required) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, t := range have {
		set[t] = struct{}{}
	}
	for _, t := range required {
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}

// IncRunning/DecRunning let the scheduler mark a job as being handled by an
// agent so capacity accounting stays honest. The scheduler bumps on dispatch,
// the result handler (C5) will decrement on final JobResult.
func (s *Session) IncRunning() { s.running.Add(1) }
func (s *Session) DecRunning() { s.running.Add(-1) }
