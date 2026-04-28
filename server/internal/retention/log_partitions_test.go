package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestSweepLogPartitions_CreatesAhead exercises just the ensure pass:
// pointing `now` at a far-future month and asking for 2 months ahead
// should result in 3 new partitions (now, now+1m, now+2m).
func TestSweepLogPartitions_CreatesAhead(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	now := time.Date(2099, 6, 15, 12, 0, 0, 0, time.UTC)
	cleanupTestPartitions(t, pool, now, 4)

	stats := retention.SweepLogPartitions(
		context.Background(), s, now, 2 /*ahead*/, 0 /*no drop*/, silent(),
	)
	if stats.Created != 3 {
		t.Fatalf("Created = %d, want 3", stats.Created)
	}
	if stats.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0 (retention=0 disables drops)", stats.Dropped)
	}

	parts, _ := s.ListLogPartitions(context.Background())
	for i := 0; i < 3; i++ {
		want := time.Date(2099, time.Month(int(now.Month())+i), 1, 0, 0, 0, 0, time.UTC)
		if !partitionListed(parts, want) {
			t.Errorf("missing partition for %s", want)
		}
	}
}

// TestSweepLogPartitions_DropsExpired drives both halves at once.
// Anchoring `now` in 2099 with a 45-day retention turns every
// migration-pre-seeded 2026 partition into "expired" too, so the
// drop pass fires far beyond just the test's months. The Cleanup
// hook reseeds the current cohort the migration would have created
// at boot, so subsequent tests in the same binary still find a
// partition for any line they try to insert today.
func TestSweepLogPartitions_DropsExpired(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	now := time.Date(2099, 4, 15, 0, 0, 0, 0, time.UTC)
	jan := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2099, 2, 1, 0, 0, 0, 0, time.UTC)
	mar := time.Date(2099, 3, 1, 0, 0, 0, 0, time.UTC)
	apr := time.Date(2099, 4, 1, 0, 0, 0, 0, time.UTC)
	may := time.Date(2099, 5, 1, 0, 0, 0, 0, time.UTC)
	cleanupTestPartitions(t, pool, jan, 5)
	t.Cleanup(func() { reseedCurrentCohort(pool) })

	for _, m := range []time.Time{jan, feb, mar} {
		if err := s.EnsureLogPartition(ctx, m); err != nil {
			t.Fatalf("seed %s: %v", m, err)
		}
	}

	// 45 days retention from 2099-04-15:
	//   cutoff = 2099-03-01
	//   jan upper (Feb 1) <= cutoff -> drop
	//   feb upper (Mar 1) <= cutoff -> drop  (cutoff is exclusive
	//                                          per !End.After(cutoff))
	//   mar upper (Apr 1)  > cutoff -> keep
	retention.SweepLogPartitions(
		ctx, s, now, 1 /*ahead -> apr+may*/, 45*24*time.Hour, silent(),
	)

	parts, _ := s.ListLogPartitions(ctx)
	if partitionListed(parts, jan) || partitionListed(parts, feb) {
		t.Fatalf("expired partitions still present: %+v", parts)
	}
	if !partitionListed(parts, mar) {
		t.Fatalf("mar partition wrongly dropped (upper > cutoff): %+v", parts)
	}
	// Ensure pass should have created apr + may at the same tick.
	if !partitionListed(parts, apr) || !partitionListed(parts, may) {
		t.Fatalf("ensure-ahead missed a month: apr=%v may=%v",
			partitionListed(parts, apr), partitionListed(parts, may))
	}
}

// reseedCurrentCohort recreates the partitions the goose migration
// pre-seeds at boot — current month plus 12 ahead — using the same
// EnsureLogPartition the production cron uses. Idempotent.
func reseedCurrentCohort(pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.New(pool)
	cur := time.Now().UTC()
	cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 12; i++ {
		_ = s.EnsureLogPartition(ctx, cur.AddDate(0, i, 0))
	}
}

func partitionListed(parts []store.LogPartition, m time.Time) bool {
	for _, p := range parts {
		if p.Start.Equal(m) {
			return true
		}
	}
	return false
}

// cleanupTestPartitions wipes a window of N months starting at `m0`
// before AND after the test, so a previous run's leftovers never
// influence the assertions and we don't leak partitions across tests.
func cleanupTestPartitions(t *testing.T, pool *pgxpool.Pool, m0 time.Time, n int) {
	t.Helper()
	wipe := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for i := 0; i < n; i++ {
			m := time.Date(m0.Year(), m0.Month()+time.Month(i), 1, 0, 0, 0, 0, time.UTC)
			name := "log_lines_y" + m.Format("2006") + "m" + m.Format("01")
			_, _ = pool.Exec(ctx,
				"ALTER TABLE log_lines DETACH PARTITION "+name)
			_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+name)
		}
	}
	wipe()
	t.Cleanup(wipe)
}

