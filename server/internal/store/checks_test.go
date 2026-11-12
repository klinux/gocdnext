package store_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// The advisory lock must give mutual exclusion for the SAME run: only one
// critical section runs at a time. This is what closes the reopen-vs-stale-
// completion window — read+PATCH can't interleave with another check op.
func TestWithRunCheckLock_SerializesSameRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID := uuid.New()

	var inside, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.WithRunCheckLock(context.Background(), runID, func() error {
				n := inside.Add(1)
				for {
					m := peak.Load()
					if n <= m || peak.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond) // widen any overlap window
				inside.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrency inside lock = %d, want 1 (mutual exclusion)", got)
	}
}

// Distinct runs must NOT serialize against each other — the lock is keyed
// per run, so unrelated check updates stay parallel.
func TestWithRunCheckLock_DistinctRunsDoNotBlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	// Explicitly distinct lock keys (first 4 bytes differ) so the test is
	// deterministic, not relying on random-uuid key separation.
	var a, b uuid.UUID
	a[0], b[0] = 1, 2

	reached := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(id uuid.UUID) {
		_ = s.WithRunCheckLock(context.Background(), id, func() error {
			reached <- struct{}{}
			<-release
			return nil
		})
	}
	go run(a)
	go run(b)

	for i := 0; i < 2; i++ {
		select {
		case <-reached:
		case <-time.After(3 * time.Second):
			t.Fatal("distinct-run locks blocked each other (should be parallel)")
		}
	}
	close(release)
}

// A newly-applied project defaults to commit_status reporting (Woodpecker/GoCD
// style) — migration 00095 flipped the column default from 'both'. The create
// path never writes the column, so the row inherits the DB default; this pins
// that the default is the single-status mode, not the old double-report 'both'.
func TestProjectCheckReporting_DefaultsToCommitStatus(t *testing.T) {
	t.Parallel()
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "freshproj", Name: "freshproj"}); err != nil {
		t.Fatalf("apply project: %v", err)
	}
	mode, err := s.GetProjectCheckReportingBySlug(ctx, "freshproj")
	if err != nil {
		t.Fatalf("get mode: %v", err)
	}
	if mode != store.CheckReportingCommitStatus {
		t.Errorf("new project check reporting mode = %q, want %q", mode, store.CheckReportingCommitStatus)
	}
}
