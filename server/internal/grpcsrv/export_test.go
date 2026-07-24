package grpcsrv

// export_test.go surfaces a handful of package-private flags so
// regression tests in grpcsrv_test can assert state transitions
// without race-prone double-bookkeeping. Lives in _test.go so
// nothing leaks into production binaries.

// SupersededByRegisterForTest reads the supersededByRegister flag
// the Connect-handler defer relies on to decide MarkAgentOffline.
// Tests use it to lock in the dual-set strategy (RevokeForAgent +
// CreateSession's internal revoke both flip this).
func (s *Session) SupersededByRegisterForTest() bool {
	return s.supersededByRegister.Load()
}

// RevokedForTest reads the revoked flag so tests can assert
// session-state transitions without reaching into atomic.Bool
// directly.
func (s *Session) RevokedForTest() bool {
	return s.revoked.Load()
}

// ReadyForTest reports whether the session reached the `ready` state
// (Connect claimed it and started its pump).
func (s *Session) ReadyForTest() bool {
	return s.ready()
}

// ReadyCountForTest is the per-store count of ready-and-not-revoked sessions —
// exactly what the AgentsOnline gauge is Set to. Tests assert this instead of the
// process-global gauge, which parallel tests would race on.
func (s *SessionStore) ReadyCountForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyCountLocked()
}

// MarkReadyForTest drives a freshly-created session through the Connect
// lifecycle (ClaimConnect → MarkReady) so scheduling tests can select it
// without standing up a real Connect stream. Panics if the session isn't
// claimable/markable (a test bug).
func (s *SessionStore) MarkReadyForTest(sessionID string) {
	if _, err := s.ClaimConnect(sessionID); err != nil {
		panic("MarkReadyForTest: ClaimConnect: " + err.Error())
	}
	if err := s.MarkReady(sessionID); err != nil {
		panic("MarkReadyForTest: MarkReady: " + err.Error())
	}
}
