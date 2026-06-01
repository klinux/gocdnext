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
