package store

// export_test.go exposes a few package-private entry points for
// regression tests that need to drive the snapshot-CAS predicates
// directly (without the List step filtering by the stale snapshot
// and short-circuiting the test before the CAS even runs). Lives in
// _test.go so it doesn't bleed into production binaries.

import (
	"context"

	"github.com/google/uuid"
)

// RequestStaleJobForTest wraps the package-private requeueStaleJob
// so test code in store_test can exercise the snapshot CAS inside
// ReclaimJobForRetry against a deliberately-stale (expectedAttempt,
// expectedAgentID) pair. Behaviour is identical to the production
// callers — no test-only branches inside.
func (s *Store) RequeueStaleJobForTest(
	ctx context.Context,
	jobID uuid.UUID,
	maxAttempts, expectedAttempt int32,
	expectedAgentID uuid.UUID,
	notify bool,
	res *ReclaimResult,
) error {
	return s.requeueStaleJob(ctx, jobID, maxAttempts, expectedAttempt, expectedAgentID, notify, res)
}

// FailJobIfStaleForTest exposes the cap-exceeded snapshot-CAS path
// for the same reason RequeueStaleJobForTest exists.
func (s *Store) FailJobIfStaleForTest(
	ctx context.Context,
	jobID uuid.UUID,
	expectedAttempt int32,
	expectedAgentID uuid.UUID,
	reason string,
) (JobCompletion, bool, error) {
	return s.failJobIfStale(ctx, jobID, expectedAttempt, expectedAgentID, reason)
}
