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
	// ErrConnectClaimed means a Connect stream tried to claim a session that is
	// no longer `registered` — a second concurrent Connect on the same session.
	// The second stream must NOT start a send pump (it would split sess.out).
	ErrConnectClaimed = errors.New("grpcsrv: session already claimed by a connect stream")
)

// connState is the session connect lifecycle.
type connState int32

const (
	stateRegistered connState = iota // published at Register; not yet schedulable
	stateConnecting                  // a Connect stream claimed it; pump starting
	stateReady                       // pump live; schedulable
)

const defaultSendBuffer = 16

// Session represents one live agent stream. The Connect handler drains `Out`
// onto the gRPC stream, the scheduler writes to it via Dispatch.
type Session struct {
	ID       string
	AgentID  uuid.UUID
	Tags     []string
	Capacity int32
	// Engine is the agent's announced execution engine name —
	// "kubernetes", "docker", "shell", or "" for legacy agents
	// that don't announce. Used by AllAgentIDs's engine filter so
	// the run-terminal CleanupRunServices broadcast targets only
	// k8s-capable agents (Docker/Shell would no-op the cleanup).
	// Empty value passes the filter — defensive fallback for
	// pre-v0.4.35 agents in a rolling upgrade window.
	Engine string
	// generation is the agents.session_generation value captured at
	// register time. The Connect handler's defer passes this back to
	// MarkAgentOffline as `observed_generation`; the SQL only flips
	// the row offline if it still matches, so a successor Register
	// that bumped the counter neutralises this defer's offline mark.
	// See migration 00033 for the full race walk.
	//
	// UNEXPORTED + IMMUTABLE post-construction: any external write
	// (the prior `sess.Generation = X` after CreateSession returned)
	// is a data race because FenceStaleSession reads this field from
	// the reaper goroutine. CreateSession initialises it in the
	// struct literal BEFORE publishing into byID/latestByAg, so the
	// mu lock + happens-before chain make every read race-free
	// without needing atomics.
	generation int64

	out     chan *gocdnextv1.ServerMessage
	running atomic.Int32
	revoked atomic.Bool
	// state is the connect lifecycle: registered → connecting → ready.
	// A session is PUBLISHED (in byID/latestByAg) at Register/CreateSession
	// but stays `registered` — NOT schedulable — until Connect claims it and
	// starts its send pump. Scheduling (FindIdleWithTags, DispatchAssignment)
	// requires `ready`, so a session with no consumer can never receive a job
	// that would sit `running` until the reaper. All transitions are CAS'd
	// under s.mu (ClaimConnect, MarkReady); reads are under s.mu too.
	state atomic.Int32
	// supersededByRegister is the signal a Connect-handler defer
	// uses to decide whether to MarkAgentOffline. When the
	// successor agent's Register flow revoked us (RevokeForAgent
	// path or CreateSession's internal revoke), this is true and
	// the defer MUST skip the offline mark — otherwise the
	// freshly-online successor's agents row gets clobbered back
	// to offline. Normal client-driven disconnects leave this
	// false; the defer marks offline as before.
	supersededByRegister atomic.Bool

	// assignedJobs is the per-session record of (job_run_id →
	// attempt) the scheduler stamped on dispatch. The result
	// handler reads this back to feed CompleteJob's snapshot CAS
	// — a stale revoked-session result for an already-redispatched
	// job has the OLD attempt, so the SQL won't match even if the
	// agent_id check happens to align (k8s rolling restart =
	// same agent UUID).
	//
	// sync.Map is fine here: writes happen once per dispatch
	// (scheduler goroutine), reads happen once per result
	// (recv-loop goroutine). No bulk iteration.
	assignedJobs sync.Map // map[uuid.UUID]int32
}

// RecordAssignment stamps the scheduler's just-dispatched (job, attempt)
// pair onto the session so the result handler can validate the snapshot
// when the agent reports back. Called by the scheduler right after
// AssignJob succeeds — sequentially in the same dispatch goroutine, no
// race with the recv-loop read because the agent can't possibly send a
// result before it sees the Assign frame.
//
// Production code MUST go through DispatchAssignment (which uses
// RecordAssignmentCAS) — bare RecordAssignment overwrites any
// existing entry unconditionally and is retained only for test
// scaffolding that needs to plant a specific snapshot.
func (s *Session) RecordAssignment(jobID uuid.UUID, attempt int32) {
	s.assignedJobs.Store(jobID, attempt)
}

// RecordAssignmentCAS is the safe-overwrite variant DispatchAssignment
// uses on the production path. Stores (jobID → attempt) ONLY when:
//   - no entry exists for jobID yet (first dispatch), OR
//   - an entry exists with the SAME attempt (idempotent retry).
//
// Returns false when an entry exists with a DIFFERENT attempt — the
// signal that something raced ahead of us (reaper requeue without a
// session fence in flight, redispatch landing before the old result
// drained). Overwriting would conflate the old attempt's eventual
// JobResult/logs with the new attempt's row identity: a late
// JobResult from attempt N would look up the recorded attempt, find
// N+1, and the snapshot CAS on CompleteJob would match the new row
// and complete it with the old payload.
//
// LoadOrStore is the right primitive here: if the key is absent it
// stores `attempt` and returns (newly-stored value, false). If the
// key is present it returns (existing value, true) WITHOUT storing,
// so a CAS failure leaves the stale entry intact for the result
// handler to validate against.
func (s *Session) RecordAssignmentCAS(jobID uuid.UUID, attempt int32) bool {
	existing, loaded := s.assignedJobs.LoadOrStore(jobID, attempt)
	if !loaded {
		return true
	}
	return existing.(int32) == attempt
}

// LookupAssignment returns the recorded attempt for a job if this
// session has one. ok=false signals the session does not own the
// job — caller should drop the message rather than process it.
// Used by handleJobResult / handleLogLine / handleTestResultBatch
// to keep stale-session traffic from rewriting state that has been
// reassigned out from under us.
func (s *Session) LookupAssignment(jobID uuid.UUID) (int32, bool) {
	v, ok := s.assignedJobs.Load(jobID)
	if !ok {
		return 0, false
	}
	return v.(int32), true
}

// ClearAssignment drops the entry once a terminal JobResult has
// been processed. Keeps the map bounded — a long-lived session
// accumulates one entry per job otherwise, and the recv-loop reads
// LookupAssignment on every job-scoped message.
func (s *Session) ClearAssignment(jobID uuid.UUID) {
	s.assignedJobs.Delete(jobID)
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
//
// Passes generation=0 — fine for tests/legacy callers that don't
// exercise the reaper-fence CAS (FenceStaleSession would still
// compare against 0 here, but those tests don't trigger it).
func (s *SessionStore) Create(agentID uuid.UUID) string {
	return s.CreateSession(agentID, nil, 1, 0).ID
}

// CreateSessionOpts is the optional metadata bag that
// CreateSessionWith threads onto the new Session. Callers without
// metadata can keep using CreateSession (zero-valued opts).
type CreateSessionOpts struct {
	Engine string
}

// CreateSession issues a session and invalidates any previous session the
// agent had — a re-registration supersedes a zombie stream that might still
// be reading with stale credentials. The returned Session owns a fresh send
// queue that Revoke will close.
//
// `generation` is the value MarkAgentOnline returned to the caller (or 0
// for tests that don't go through the agents table). It MUST be passed
// here, not assigned to the returned Session afterwards: post-create
// writes to the field race with reaper-side FenceStaleSession reads
// AND opened a window where the session was published with the wrong
// value (round 12 HIGH). Initialising inside the struct literal under
// the mu lock makes every later read race-free via the happens-before
// chain (publish-under-lock → lookup-under-lock → safe to read
// immutable field).
func (s *SessionStore) CreateSession(agentID uuid.UUID, tags []string, capacity int32, generation int64, opts ...CreateSessionOpts) *Session {
	if capacity <= 0 {
		capacity = 1
	}
	var meta CreateSessionOpts
	if len(opts) > 0 {
		meta = opts[0]
	}
	sess := &Session{
		ID:         uuid.NewString(),
		AgentID:    agentID,
		Tags:       append([]string(nil), tags...),
		Capacity:   capacity,
		Engine:     meta.Engine,
		generation: generation,
		out:        make(chan *gocdnextv1.ServerMessage, defaultSendBuffer),
	}

	s.mu.Lock()
	if prev, ok := s.latestByAg[agentID]; ok {
		if prevSess, ok := s.byID[prev]; ok {
			// Mark superseded BEFORE we drop the revoked flag so
			// the Connect handler's defer (which races with this
			// CreateSession from the OLD stream's goroutine) sees
			// the "don't mark offline" signal regardless of which
			// flag check it reads first.
			prevSess.supersededByRegister.Store(true)
			prevSess.revoked.Store(true)
			close(prevSess.out)
		}
		delete(s.byID, prev)
	}
	s.byID[sess.ID] = sess
	s.latestByAg[agentID] = sess.ID
	// The new session is born `registered` (not ready), so it does NOT count as
	// online and is NOT schedulable yet — Connect's MarkReady does both. A prev
	// session revoked just above may have been ready, so recompute the gauge.
	gauge := float64(s.readyCountLocked())
	s.mu.Unlock()
	metrics.AgentsOnline.Set(gauge)

	// onReady is deliberately NOT fired here — it fires from MarkReady, when the
	// agent actually has a live consumer for sess.out. Firing it at Register let
	// the scheduler dispatch into a session with no reader (jobs stuck running).
	return sess
}

// readyCountLocked counts live (current, not-revoked) sessions in the `ready`
// state — the true "online" figure. Caller holds s.mu.
func (s *SessionStore) readyCountLocked() int {
	n := 0
	for _, id := range s.latestByAg {
		if sess := s.byID[id]; sess != nil && !sess.revoked.Load() && sess.ready() {
			n++
		}
	}
	return n
}

// ready reports whether the session's Connect stream is live and it may be
// scheduled. Reads the CAS'd state.
func (s *Session) ready() bool { return connState(s.state.Load()) == stateReady }

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

// RevokeForAgent revokes whatever session is currently associated with
// the given agent, if any. Used by the Register handler to fence off
// the OLD session BEFORE the register-fence requeues running jobs:
// without this, the scheduler could wake from the requeue's NOTIFY,
// see the still-live old session as idle, and re-dispatch the
// just-requeued job to it — undoing the fence and re-orphaning the
// row. Safe on missing entries (idempotent).
//
// Sets supersededByRegister=true on the closing session so the
// Connect handler's defer knows not to MarkAgentOffline when its
// recv loop eventually exits — the successor's Register will (or
// already did) flip the agents row to online, and a stray offline
// mark from the obsolete defer would clobber it.
func (s *SessionStore) RevokeForAgent(agentID uuid.UUID) {
	s.mu.Lock()
	id, ok := s.latestByAg[agentID]
	var sess *Session
	if ok {
		sess = s.byID[id]
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if sess != nil {
		sess.supersededByRegister.Store(true)
	}
	s.Revoke(id)
}

// FenceResult tells the reaper which branch FenceStaleSession took
// so the sweep log can distinguish "agent had no live session"
// (normal: process already exited cleanly) from "a successor took
// over before we got here" (race) from "we actually revoked the
// stale session" (the load-bearing outcome). All three are valid;
// the differentiation matters for operational investigation, not
// correctness.
type FenceResult int

const (
	// FenceResultRevoked means the live session's generation matched
	// the snapshot and we closed it — the load-bearing outcome.
	FenceResultRevoked FenceResult = iota
	// FenceResultNoSession means there was nothing to fence; the
	// agent had already disconnected by the time we got here.
	FenceResultNoSession
	// FenceResultGenerationChanged means a successor Register raced
	// ahead of the reaper between SELECT and fence. Successor is
	// preserved; the reaper's notify will reach it as the fresh
	// available agent for the requeued work.
	FenceResultGenerationChanged
)

// FenceStaleSession is the reaper-side variant of RevokeForAgent.
// Two differences from RevokeForAgent:
//
//  1. CAS by generation. The reaper observed `observedGeneration`
//     in `agents.session_generation` at SELECT time. Between SELECT
//     and now, the agent may have re-Registered — bumping the
//     counter and creating a NEW healthy session. Revoking by
//     agentID alone would kill the freshly-online successor.
//     This method ONLY revokes when the live session's Generation
//     still matches the observed snapshot; mismatch → no-op.
//
//  2. supersededByRegister is NOT set. RevokeForAgent uses that
//     flag so the closing session's Connect defer skips
//     MarkAgentOffline (Register will MarkAgentOnline anyway).
//     The reaper path has no successor Register coming —
//     the agent really is dead from our POV, and the defer SHOULD
//     mark the row offline so the agents view reflects reality.
//     The defer's MarkAgentOffline uses CAS-by-generation itself,
//     so a successor Register that DOES come along later won't be
//     clobbered.
//
// Safe on missing entries (idempotent). Returns a FenceResult so
// the caller can log the three outcomes distinctly: revoked /
// no-session / generation-changed. Correctness doesn't depend on
// the differentiation; operational visibility does.
func (s *SessionStore) FenceStaleSession(agentID uuid.UUID, observedGeneration int64) FenceResult {
	s.mu.Lock()
	id, ok := s.latestByAg[agentID]
	var sess *Session
	if ok {
		sess = s.byID[id]
	}
	s.mu.Unlock()
	if !ok || sess == nil {
		return FenceResultNoSession
	}
	// Generation check happens outside the mu lock — sess.generation
	// is set once in the CreateSession struct literal (under mu) and
	// never mutated afterward, so the read is race-free via the
	// happens-before chain (publish-under-lock → lookup-under-lock).
	// A concurrent CreateSession would have already pointed
	// latestByAg at a DIFFERENT session id; the id we read above
	// would be stale, but the sess pointer we captured is the
	// matching one. Either we hit the right (now-removed-from-latest)
	// session OR we read the NEW session and its generation !=
	// observedGeneration → no-op.
	if sess.generation != observedGeneration {
		return FenceResultGenerationChanged
	}
	s.Revoke(id)
	return FenceResultRevoked
}

// Generation returns the per-agent monotonic epoch counter that
// agents.session_generation held when this session was created.
// Immutable for the session's lifetime — readers (Connect defer's
// MarkAgentOffline, reaper's FenceStaleSession CAS) can rely on
// the value not changing under them.
func (s *Session) Generation() int64 { return s.generation }

// IsAgentSuperseded reports whether the SessionStore currently has a
// session for `agentID` whose ID is NOT `closingSessionID`. Used by
// the Connect handler's defer to decide if a stream close should
// also flip agents.status='offline' in the DB.
//
// Without this guard, an old stream that closes AFTER a successor
// register would mark the freshly-online agent offline — and the
// reaper, treating any offline agent as stale regardless of
// heartbeat freshness, would reclaim the new session's healthy
// jobs.
//
// Returns false when (a) there's no session at all (normal
// disconnect that already ran through Revoke's latestByAg cleanup)
// OR (b) the latest session IS the closing one (race-free terminal
// state). Returns true only when a DIFFERENT, live session has
// taken over the slot — meaning the agent is online via that
// successor and our defer must skip MarkAgentOffline.
func (s *SessionStore) IsAgentSuperseded(agentID uuid.UUID, closingSessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, ok := s.latestByAg[agentID]
	return ok && latest != closingSessionID
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
	gauge := float64(s.readyCountLocked())
	s.mu.Unlock()
	metrics.AgentsOnline.Set(gauge)
}

// ClaimConnect transitions a published session from `registered` to
// `connecting`, exclusively — the FIRST Connect stream for a session wins and
// starts the send pump; a SECOND concurrent Connect gets ErrConnectClaimed and
// must not pump (two pumps would split sess.out). It also validates the session
// is still the agent's current one and not revoked.
func (s *SessionStore) ClaimConnect(sessionID string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[sessionID]
	if !ok || sess.revoked.Load() {
		return nil, ErrNoSession
	}
	if cur, ok := s.latestByAg[sess.AgentID]; !ok || cur != sessionID {
		return nil, ErrNoSession // superseded by a newer Register
	}
	if !sess.state.CompareAndSwap(int32(stateRegistered), int32(stateConnecting)) {
		return nil, ErrConnectClaimed
	}
	return sess, nil
}

// MarkReady transitions a claimed session from `connecting` to `ready` — call it
// AFTER the send pump has started, so a scheduled dispatch always finds a live
// consumer. It fires onReady once (the scheduler drains its queued-run backlog)
// and refreshes the online gauge. A session that was revoked/superseded between
// ClaimConnect and here fails the CAS and returns ErrNoSession.
func (s *SessionStore) MarkReady(sessionID string) error {
	s.mu.Lock()
	sess, ok := s.byID[sessionID]
	if !ok || sess.revoked.Load() {
		s.mu.Unlock()
		return ErrNoSession
	}
	if !sess.state.CompareAndSwap(int32(stateConnecting), int32(stateReady)) {
		s.mu.Unlock()
		return ErrNoSession
	}
	onReady := s.onReady
	gauge := float64(s.readyCountLocked())
	s.mu.Unlock()

	metrics.AgentsOnline.Set(gauge)
	// Outside the lock so a slow subscriber (scheduler draining the DB) can't
	// stall the Connect handler.
	if onReady != nil {
		go onReady()
	}
	return nil
}

// Dispatch enqueues msg onto the agent's current session. Returns
// ErrNoSession if the agent is not connected, ErrSessionBusy if the
// queue is full. Callers should treat busy as "try another agent".
//
// Holds the store mutex through the channel send. That's atypical
// but necessary: Revoke also takes the mutex before close(sess.out),
// so doing the send inside the lock prevents a send-on-closed-channel
// panic when a concurrent Revoke races us. The channel has a small
// buffer (defaultSendBuffer=16) and we `select default` for the
// busy case, so the lock-held duration is bounded.
//
// For Assign messages, use DispatchAssignment instead — it records
// the (jobID, attempt) snapshot on the same session that receives
// the frame, atomically. Calling Dispatch with an Assign won't
// stamp the assignment and the eventual JobResult will be dropped
// as "no assignment for this job".
func (s *SessionStore) Dispatch(agentID uuid.UUID, msg *gocdnextv1.ServerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.latestByAg[agentID]
	if !ok {
		return ErrNoSession
	}
	sess := s.byID[id]
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

// DispatchAssignment is the atomic record-and-dispatch path the
// scheduler uses for Assign messages. The two operations
//  1. Session.RecordAssignment(jobID, attempt)
//  2. send msg onto sess.out
//
// MUST happen on the SAME session — without atomicity, a successor
// Register firing between the lookup-for-record and the lookup-
// for-dispatch could move the agent to a new session. Recording
// stamps session A; dispatch enqueues on session B; the agent
// (running on B's stream) reports a result B has no assignment
// for and the result handler drops it.
//
// Holds the store mutex throughout, just like Dispatch — same
// reasoning, plus this guarantees the recorded assignment lives
// on whatever session actually receives the frame.
func (s *SessionStore) DispatchAssignment(
	agentID uuid.UUID,
	msg *gocdnextv1.ServerMessage,
	jobID uuid.UUID,
	attempt int32,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.latestByAg[agentID]
	if !ok {
		return ErrNoSession
	}
	sess := s.byID[id]
	if sess == nil || sess.revoked.Load() {
		return ErrNoSession
	}
	// A published-but-not-yet-`ready` session has no consumer for sess.out; a
	// dispatch here would sit `running` until the reaper. Selection also skips
	// it (FindIdleWithTags), but this closes the select→dispatch window.
	if !sess.ready() {
		return ErrNoSession
	}
	// Record BEFORE the channel write so the recv side cannot
	// observe an Assign without the matching assignment entry
	// (the goroutine reading sess.out can't proceed until we
	// release the mutex, but it doesn't take the SessionStore
	// mutex — only sess.assignedJobs's internal sync.Map mutex,
	// which RecordAssignmentCAS finished by then).
	//
	// CAS rejection: if this session already holds a DIFFERENT
	// attempt for jobID, refuse the dispatch as busy. This is
	// the trip-wire for the "reaper requeue with notify-before-
	// fence" race: the in-memory session of a stale agent must
	// not be allowed to silently accept a redispatch under a new
	// attempt while still holding the old one. The fence ordering
	// in Reaper.Sweep is the primary defense; this is the safety
	// net for misconfigured / test paths / future regressions.
	if !sess.RecordAssignmentCAS(jobID, attempt) {
		return ErrSessionBusy
	}
	select {
	case sess.out <- msg:
		if msg.GetAssign() != nil {
			sess.IncRunning()
		}
		return nil
	default:
		// Roll back the record so a scheduler retry can re-stamp
		// cleanly without leaving a phantom assignment. Only roll
		// back if we actually planted THIS attempt (idempotent
		// re-record path returned true without writing — we'd
		// otherwise nuke a healthy concurrent entry).
		if existing, ok := sess.LookupAssignment(jobID); ok && existing == attempt {
			sess.ClearAssignment(jobID)
		}
		return ErrSessionBusy
	}
}

// AllAgentIDs returns every agent currently holding a session,
// optionally filtered to a specific engine name. Used by the
// run-terminal CleanupRunServices broadcast to widen the target
// set beyond "agents that ran a job of this run": the k8s agent
// that created the pods may have disconnected, but ANY other k8s
// agent in the cluster (online right now, even if not part of
// this run) can do the label-selector delete since the pods
// live in a cluster-wide namespace.
//
// engineFilter empty = no filter (returns every connected agent
// — preserves the v0.4.34 behaviour). Setting a value (e.g.
// "kubernetes") restricts the result to sessions whose Engine
// field matches AND sessions whose Engine is empty (legacy
// agents from before the Register-engine field shipped — included
// defensively so a rolling upgrade window doesn't lose cleanup
// coverage). Anything else (Docker/Shell with known names) is
// excluded — those engines no-op the cleanup, and broadcasting
// to them at scale wastes wire + log noise.
//
// Order is unspecified (Go map iteration). Callers DEDUPE against
// their primary set before dispatching.
func (s *SessionStore) AllAgentIDs(engineFilter string) []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uuid.UUID, 0, len(s.latestByAg))
	for agentID, sessID := range s.latestByAg {
		if engineFilter != "" {
			sess, ok := s.byID[sessID]
			if !ok {
				continue
			}
			// Empty Engine = unknown (pre-v0.4.35 agent); include
			// defensively. Otherwise strict equality.
			if sess.Engine != "" && sess.Engine != engineFilter {
				continue
			}
		}
		out = append(out, agentID)
	}
	return out
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
		if !sess.ready() {
			continue // published at Register but no live Connect stream yet
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

// Running exposes the live in-flight count for the metrics collector. Read-only
// snapshot of the atomic; callers must not assume it is stable across calls.
func (s *Session) Running() int32 { return s.running.Load() }

// AgentCapacitySample is one live agent session's running/capacity pair.
type AgentCapacitySample struct {
	AgentID  uuid.UUID
	Running  int32
	Capacity int32
}

// CapacitySnapshot returns running/capacity for every LIVE (non-revoked, ready)
// session, taken under the lock and returned by value — so the metrics
// collector can emit to its channel WITHOUT holding s.mu (which dispatch and
// the idle scan also take). A revoked/dead OR not-yet-`ready` session is skipped
// (the latter is not live capacity), so its series disappears/never appears
// rather than lingering as phantom capacity.
func (s *SessionStore) CapacitySnapshot() []AgentCapacitySample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AgentCapacitySample, 0, len(s.latestByAg))
	for _, id := range s.latestByAg {
		sess, ok := s.byID[id]
		if !ok || sess.revoked.Load() || !sess.ready() {
			continue
		}
		out = append(out, AgentCapacitySample{
			AgentID:  sess.AgentID,
			Running:  sess.running.Load(),
			Capacity: sess.Capacity,
		})
	}
	return out
}
