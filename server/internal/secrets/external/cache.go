package external

import (
	"sync"
	"time"
)

// TTLCache is a small, bounded, TTL cache for external lookups so a fan-out
// of N jobs referencing the same path hits the backend once. Plaintext lives
// in memory only — never logged, never persisted. ErrSecretNotFound is never
// cached (a just-created/rotated secret must resolve immediately). ttl<=0
// disables caching entirely (zero-staleness deployments).
type TTLCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	max     int
	entries map[string]cacheEntry
	now     func() time.Time // injectable for tests
}

type cacheEntry struct {
	value  string
	expiry time.Time
}

// NewTTLCache builds a cache. A non-positive ttl yields a no-op cache.
func NewTTLCache(ttl time.Duration, max int) *TTLCache {
	if max <= 0 {
		max = 4096
	}
	return &TTLCache{ttl: ttl, max: max, entries: map[string]cacheEntry{}, now: time.Now}
}

// CacheKey namespaces by source so two backends can't collide on a path.
func CacheKey(source, path, key string) string {
	return source + "\x00" + path + "\x00" + key
}

// Get returns the cached value if present and unexpired.
func (c *TTLCache) Get(k string) (string, bool) {
	if c == nil || c.ttl <= 0 {
		return "", false
	}
	c.mu.RLock()
	e, ok := c.entries[k]
	c.mu.RUnlock()
	if !ok || c.now().After(e.expiry) {
		return "", false
	}
	return e.value, true
}

// Put stores a value with the configured TTL. Bounded: it purges expired
// entries on write and, if still at cap, resets (the cache is an
// optimization, not a store — dropping it only costs a re-fetch).
func (c *TTLCache) Put(k, v string) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		now := c.now()
		for key, e := range c.entries {
			if now.After(e.expiry) {
				delete(c.entries, key)
			}
		}
		if len(c.entries) >= c.max {
			c.entries = make(map[string]cacheEntry, c.max)
		}
	}
	c.entries[k] = cacheEntry{value: v, expiry: c.now().Add(c.ttl)}
}
