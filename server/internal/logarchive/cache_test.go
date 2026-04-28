package logarchive_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/logarchive"
)

func mkLines(n int, textSize int) []logarchive.Line {
	out := make([]logarchive.Line, n)
	at := time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC)
	body := strings.Repeat("x", textSize)
	for i := range out {
		out[i] = logarchive.Line{
			Seq: int64(i + 1), At: at, Stream: "stdout", Text: body,
		}
	}
	return out
}

func TestLineCache_HitMiss(t *testing.T) {
	c := logarchive.NewLineCache(10 * 1024 * 1024)
	lines := mkLines(5, 16)

	if _, ok := c.Get("k"); ok {
		t.Fatal("Get on empty cache returned hit")
	}
	c.Put("k", lines)
	got, ok := c.Get("k")
	if !ok || len(got) != 5 {
		t.Fatalf("Get after Put: ok=%v len=%d", ok, len(got))
	}
}

// TestLineCache_LRUEvicts checks that putting more bytes than the
// budget allows evicts least-recently-used entries first.
func TestLineCache_LRUEvicts(t *testing.T) {
	// Each `mkLines(10, 1024)` is ~10 KiB raw + per-line overhead.
	// Budget of 30 KiB lets ~2 entries coexist; the third evicts
	// the oldest.
	c := logarchive.NewLineCache(30 * 1024)

	c.Put("a", mkLines(10, 1024))
	c.Put("b", mkLines(10, 1024))
	// Touch a so b becomes oldest.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("missing a after put")
	}
	c.Put("c", mkLines(10, 1024))

	if _, ok := c.Get("a"); !ok {
		t.Errorf("a was evicted, expected to survive (recently used)")
	}
	if _, ok := c.Get("b"); ok {
		t.Errorf("b not evicted; LRU broken")
	}
	if _, ok := c.Get("c"); !ok {
		t.Errorf("c missing after Put")
	}
}

func TestLineCache_ZeroBudgetIsNoOp(t *testing.T) {
	c := logarchive.NewLineCache(0)
	c.Put("k", mkLines(3, 16))
	if _, ok := c.Get("k"); ok {
		t.Errorf("zero-budget cache should not retain entries")
	}
	if s := c.Stats(); s.Entries != 0 {
		t.Errorf("Stats.Entries = %d, want 0", s.Entries)
	}
}

func TestLineCache_ReplaceUpdatesBytes(t *testing.T) {
	c := logarchive.NewLineCache(10 * 1024 * 1024)
	c.Put("k", mkLines(3, 16))
	first := c.Stats().Bytes
	// Replace with a much bigger payload — bytes must reflect the
	// new size, not be additively wrong.
	c.Put("k", mkLines(50, 32))
	second := c.Stats().Bytes
	if second <= first {
		t.Errorf("expected bytes to grow on replace: first=%d second=%d", first, second)
	}
}
