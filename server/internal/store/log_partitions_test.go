package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Far-future months avoid colliding with the migration's pre-created
// 2026-cohort partitions (current + 12 ahead at boot). Tests drop
// what they create in t.Cleanup so a subsequent run sees a clean
// catalogue — the dbtest TRUNCATE pass doesn't know about partition
// children added at runtime.
var (
	testMonthA = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	testMonthB = time.Date(2099, 2, 1, 0, 0, 0, 0, time.UTC)
	testMonthC = time.Date(2099, 3, 1, 0, 0, 0, 0, time.UTC)
)

func TestEnsureLogPartition_IsIdempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	cleanupPartitions(t, pool, testMonthA)

	if err := s.EnsureLogPartition(ctx, testMonthA); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// Second call must be a no-op error-wise — the probe sees the
	// partition and skips, racing callers fall through to the
	// 42P07-tolerant CREATE.
	if err := s.EnsureLogPartition(ctx, testMonthA); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	parts, err := s.ListLogPartitions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !containsPartitionFor(parts, testMonthA) {
		t.Fatalf("partition for %s not in list: %+v", testMonthA, parts)
	}
}

func TestListLogPartitions_AscendingByStart(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	cleanupPartitions(t, pool, testMonthA, testMonthB, testMonthC)

	// Create out of order to force ListLogPartitions to actually
	// sort rather than rely on whatever pg_inherits hands back.
	for _, m := range []time.Time{testMonthC, testMonthA, testMonthB} {
		if err := s.EnsureLogPartition(ctx, m); err != nil {
			t.Fatalf("ensure %s: %v", m, err)
		}
	}

	parts, err := s.ListLogPartitions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Filter to just the test months — the migration also created
	// 2026-shaped children we don't want to assert on.
	var seen []time.Time
	for _, p := range parts {
		if p.Start.Year() == 2099 {
			seen = append(seen, p.Start)
		}
	}
	if len(seen) != 3 ||
		!seen[0].Equal(testMonthA) ||
		!seen[1].Equal(testMonthB) ||
		!seen[2].Equal(testMonthC) {
		t.Fatalf("partitions order = %+v, want [A B C]", seen)
	}
}

func TestDropLogPartition_RemovesFromCatalogue(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	cleanupPartitions(t, pool, testMonthA)

	if err := s.EnsureLogPartition(ctx, testMonthA); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	parts, _ := s.ListLogPartitions(ctx)
	name := pickPartitionFor(parts, testMonthA)
	if name == "" {
		t.Fatal("partition for testMonthA not present after ensure")
	}

	if err := s.DropLogPartition(ctx, name); err != nil {
		t.Fatalf("drop: %v", err)
	}

	parts, _ = s.ListLogPartitions(ctx)
	if containsPartitionFor(parts, testMonthA) {
		t.Fatalf("partition still listed after drop: %+v", parts)
	}
}

func TestDropLogPartition_RejectsUnsafeName(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Anything that doesn't match `log_lines_y\d{4}m\d{2}` must be
	// refused before reaching the SQL — the helper builds DDL by
	// concatenation so accepting arbitrary text would be an
	// injection foothold.
	for _, bad := range []string{
		"log_lines_pre",
		"log_lines",
		"public.log_lines_y2099m01",
		"log_lines_y2099m01; DROP TABLE users",
	} {
		if err := s.DropLogPartition(ctx, bad); err == nil {
			t.Errorf("DropLogPartition(%q) returned nil, want refusal", bad)
		}
	}
}

// containsPartitionFor returns true when `parts` covers the month
// starting at `m`.
func containsPartitionFor(parts []store.LogPartition, m time.Time) bool {
	for _, p := range parts {
		if p.Start.Equal(m) {
			return true
		}
	}
	return false
}

func pickPartitionFor(parts []store.LogPartition, m time.Time) string {
	for _, p := range parts {
		if p.Start.Equal(m) {
			return p.Name
		}
	}
	return ""
}

// cleanupPartitions drops anything left from a prior test run AND
// registers a t.Cleanup hook to wipe what the current test created.
// Idempotent — DROP IF EXISTS swallows misses.
func cleanupPartitions(t *testing.T, pool *pgxpool.Pool, months ...time.Time) {
	t.Helper()
	wipe := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, m := range months {
			name := "log_lines_y" + m.UTC().Format("2006") + "m" + m.UTC().Format("01")
			// DETACH first because dropping a child from a
			// partitioned parent without detaching can fail in
			// some edge cases (FKs across versions). IF EXISTS
			// keeps the helper idempotent across both phases.
			_, _ = pool.Exec(ctx,
				"ALTER TABLE log_lines DETACH PARTITION "+name)
			_, _ = pool.Exec(ctx,
				"DROP TABLE IF EXISTS "+name)
		}
	}
	wipe()
	t.Cleanup(wipe)
}
