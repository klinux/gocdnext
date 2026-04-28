package logarchive

import (
	"container/list"
	"sync"
)

// LineCache memoises decoded archives keyed by storage key. Lookups
// after the first hit avoid both the artefact-store round-trip and
// the gunzip+parse cost — both are non-trivial on archives that
// span thousands of lines, and a typical UI session reloads the
// run-detail page several times.
//
// Eviction is bytes-bounded. Each entry's footprint is the sum of
// its lines' Text + Stream sizes plus a constant overhead per line
// (the Seq + At fields, slice/struct headers — folded into the
// constant below). When the cache exceeds the budget we evict
// least-recently-used entries until under.
//
// Archives are immutable once written, so there's no TTL — only the
// LRU window matters.
type LineCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	ll       *list.List
	idx      map[string]*list.Element
}

// perLineOverhead is what we charge each entry on top of its raw
// text bytes — Seq (8B) + At (24B-ish) + slice header (24B) +
// struct alignment. Tuned so a small archive (~100 short lines)
// reports a few KB and a "go test -v" archive reports ~MB.
const perLineOverhead = 96

type cacheEntry struct {
	key   string
	lines []Line
	bytes int64
}

// NewLineCache creates a cache with the given byte budget. A budget
// of 0 returns a usable cache that immediately evicts everything —
// effectively disabled. Negative budgets are clamped to 0.
func NewLineCache(maxBytes int64) *LineCache {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &LineCache{
		maxBytes: maxBytes,
		ll:       list.New(),
		idx:      make(map[string]*list.Element),
	}
}

// Get returns the cached lines for `key` and bumps it to the head
// of the LRU. Second return is false when the entry isn't present.
func (c *LineCache) Get(key string) ([]Line, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.idx[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(*cacheEntry).lines, true
}

// Put inserts the entry, evicting LRU entries until total bytes
// fit the budget. If a single entry exceeds the budget it's still
// stored — pathological logs (single multi-GB archive) shouldn't
// hard-fail the read; the next Put just evicts it again.
func (c *LineCache) Put(key string, lines []Line) {
	size := sizeOf(lines)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Replace existing — net delta is the difference.
	if e, ok := c.idx[key]; ok {
		old := e.Value.(*cacheEntry)
		c.bytes -= old.bytes
		old.lines = lines
		old.bytes = size
		c.bytes += size
		c.ll.MoveToFront(e)
		c.evictUntilFits()
		return
	}

	entry := &cacheEntry{key: key, lines: lines, bytes: size}
	e := c.ll.PushFront(entry)
	c.idx[key] = e
	c.bytes += size
	c.evictUntilFits()
}

// Stats returns a snapshot for ops dashboards.
type CacheStats struct {
	Entries  int
	Bytes    int64
	MaxBytes int64
}

func (c *LineCache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Entries: c.ll.Len(), Bytes: c.bytes, MaxBytes: c.maxBytes}
}

// Reset wipes everything. Mainly for tests / admin "flush" actions.
func (c *LineCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.idx = make(map[string]*list.Element)
	c.bytes = 0
}

func (c *LineCache) evictUntilFits() {
	if c.maxBytes == 0 {
		// Disabled budget: drop the entry we just inserted (and
		// anything else that's queued).
		c.ll.Init()
		c.idx = make(map[string]*list.Element)
		c.bytes = 0
		return
	}
	for c.bytes > c.maxBytes && c.ll.Len() > 1 {
		oldest := c.ll.Back()
		entry := oldest.Value.(*cacheEntry)
		c.ll.Remove(oldest)
		delete(c.idx, entry.key)
		c.bytes -= entry.bytes
	}
}

func sizeOf(lines []Line) int64 {
	var n int64
	for _, l := range lines {
		n += int64(len(l.Text)) + int64(len(l.Stream)) + perLineOverhead
	}
	return n
}
