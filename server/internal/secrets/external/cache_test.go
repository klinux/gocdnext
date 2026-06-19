package external

import (
	"testing"
	"time"
)

func TestTTLCache_HitAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewTTLCache(60*time.Second, 4)
	c.now = func() time.Time { return now }

	k := CacheKey("vault", "secret/app", "PASS")
	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache should miss")
	}
	c.Put(k, "v")
	if v, ok := c.Get(k); !ok || v != "v" {
		t.Fatalf("get within ttl = %q, %v", v, ok)
	}
	now = now.Add(61 * time.Second)
	if _, ok := c.Get(k); ok {
		t.Fatal("expired entry should miss")
	}
}

func TestTTLCache_Disabled(t *testing.T) {
	c := NewTTLCache(0, 4) // ttl<=0 disables
	k := CacheKey("aws", "p", "")
	c.Put(k, "v")
	if _, ok := c.Get(k); ok {
		t.Fatal("ttl<=0 must disable caching")
	}
}

func TestTTLCache_NamespacedBySource(t *testing.T) {
	if CacheKey("vault", "p", "k") == CacheKey("aws", "p", "k") {
		t.Fatal("cache key must namespace by source")
	}
}
