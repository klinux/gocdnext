package secrets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/secrets/external"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// countingBackend counts Fetch calls (thread-safe) and can stall for `delay`,
// respecting ctx cancellation — used to exercise singleflight dedup and the
// per-lookup timeout.
type countingBackend struct {
	name  string
	value string
	calls atomic.Int64
	delay time.Duration
}

func (b *countingBackend) Name() string                        { return b.name }
func (b *countingBackend) HealthCheck(_ context.Context) error { return nil }
func (b *countingBackend) Fetch(ctx context.Context, _, _ string) (string, error) {
	b.calls.Add(1)
	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return b.value, nil
}

// fakeBackend is an in-memory external.Backend for dispatch tests.
type fakeBackend struct {
	name  string
	data  map[string]string // path\x00key → value
	calls int
	err   error
}

func (f *fakeBackend) Name() string                        { return f.name }
func (f *fakeBackend) HealthCheck(_ context.Context) error { return f.err }
func (f *fakeBackend) Fetch(_ context.Context, path, key string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.data[path+"\x00"+key]
	if !ok {
		return "", external.ErrSecretNotFound
	}
	return v, nil
}

func newTestCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipherFromHex(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func TestCompositeResolver_Dispatch(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newTestCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	// Seed a db value, a resolvable vault ref, and a vault ref to a
	// missing path (→ silent omit).
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "DBV", Value: []byte("db-value")})
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "VREF", Source: store.SecretSourceVault, RefPath: "secret/app", RefKey: "PASS"})
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "GONE", Source: store.SecretSourceVault, RefPath: "secret/absent", RefKey: "X"})

	fake := &fakeBackend{name: "vault", data: map[string]string{"secret/app\x00PASS": "topsecret"}}
	r, err := secrets.NewCompositeResolver(secrets.CompositeConfig{
		Store:    s,
		Cipher:   cipher,
		Backends: map[string]external.Backend{store.SecretSourceVault: fake},
		Cache:    external.NewTTLCache(time.Minute, 64),
	})
	if err != nil {
		t.Fatalf("new composite: %v", err)
	}

	got, err := r.Resolve(ctx, applied.ProjectID, []string{"DBV", "VREF", "GONE", "UNKNOWN"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["DBV"] != "db-value" {
		t.Errorf("db dispatch = %q", got["DBV"])
	}
	if got["VREF"] != "topsecret" {
		t.Errorf("vault dispatch = %q", got["VREF"])
	}
	if _, ok := got["GONE"]; ok {
		t.Error("external not-found should be silently omitted")
	}
	if _, ok := got["UNKNOWN"]; ok {
		t.Error("unknown name should be omitted")
	}

	// Cache: a second resolve of the same ref hits the cache (one Fetch).
	if _, err := r.Resolve(ctx, applied.ProjectID, []string{"VREF"}); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if fake.calls != 2 {
		// 2 = one for VREF + one for GONE in the first call; the second
		// resolve of VREF is served from cache (GONE is not cached).
		t.Fatalf("backend Fetch calls = %d, want 2 (VREF cached on the 2nd resolve)", fake.calls)
	}
}

func TestCompositeResolver_UnconfiguredBackend_FailsLoud(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newTestCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "AWSREF", Source: store.SecretSourceAWS, RefPath: "prod/key"})

	// Only vault configured; the aws reference must fail loud (fail-closed),
	// citing the name, never a value.
	r, _ := secrets.NewCompositeResolver(secrets.CompositeConfig{
		Store: s, Cipher: cipher,
		Backends: map[string]external.Backend{store.SecretSourceVault: &fakeBackend{name: "vault"}},
		Cache:    external.NewTTLCache(0, 0),
	})
	_, err := r.Resolve(ctx, applied.ProjectID, []string{"AWSREF"})
	if err == nil || !strings.Contains(err.Error(), "not configured") || !strings.Contains(err.Error(), "AWSREF") {
		t.Fatalf("err = %v, want a loud not-configured error naming AWSREF", err)
	}
}

func TestCompositeResolver_BackendErrorPropagates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newTestCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "VREF", Source: store.SecretSourceVault, RefPath: "p", RefKey: "k"})

	boom := errors.New("vault sealed")
	r, _ := secrets.NewCompositeResolver(secrets.CompositeConfig{
		Store: s, Cipher: cipher,
		Backends: map[string]external.Backend{store.SecretSourceVault: &fakeBackend{name: "vault", err: boom}},
		Cache:    external.NewTTLCache(0, 0),
	})
	if _, err := r.Resolve(ctx, applied.ProjectID, []string{"VREF"}); err == nil {
		t.Fatal("a backend (non-not-found) error must propagate, not be omitted")
	}
}

// TestCompositeResolver_SingleflightDedupesConcurrentMisses pins the
// "fan-out of N jobs on the same path hits the backend once" promise: 20
// goroutines resolve the same cold-cache reference simultaneously and the
// backend must see exactly one Fetch (singleflight collapses in-flight
// callers; the inner cache re-check catches any straggler).
func TestCompositeResolver_SingleflightDedupesConcurrentMisses(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newTestCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "VREF", Source: store.SecretSourceVault, RefPath: "secret/app", RefKey: "PASS"})

	be := &countingBackend{name: "vault", value: "topsecret", delay: 30 * time.Millisecond}
	r, err := secrets.NewCompositeResolver(secrets.CompositeConfig{
		Store: s, Cipher: cipher,
		Backends: map[string]external.Backend{store.SecretSourceVault: be},
		Cache:    external.NewTTLCache(time.Minute, 64),
	})
	if err != nil {
		t.Fatalf("new composite: %v", err)
	}

	const n = 20
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, gerr := r.Resolve(ctx, applied.ProjectID, []string{"VREF"})
			if gerr != nil {
				errs <- gerr
				return
			}
			if got["VREF"] != "topsecret" {
				errs <- fmt.Errorf("VREF = %q", got["VREF"])
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent resolve: %v", e)
	}
	if c := be.calls.Load(); c != 1 {
		t.Fatalf("backend Fetch calls = %d, want 1 (singleflight + cache must collapse the fan-out)", c)
	}
}

// TestCompositeResolver_FetchTimeout pins the per-lookup deadline: a backend
// that never returns must not pin the resolver — the configured timeout
// bounds the call and the error propagates promptly.
func TestCompositeResolver_FetchTimeout(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newTestCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	mustSet(t, s, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "VREF", Source: store.SecretSourceVault, RefPath: "p", RefKey: "k"})

	be := &countingBackend{name: "vault", value: "v", delay: time.Hour} // never returns in time
	r, _ := secrets.NewCompositeResolver(secrets.CompositeConfig{
		Store: s, Cipher: cipher,
		Backends: map[string]external.Backend{store.SecretSourceVault: be},
		Cache:    external.NewTTLCache(0, 0),
		Timeout:  50 * time.Millisecond,
	})
	began := time.Now()
	_, err := r.Resolve(ctx, applied.ProjectID, []string{"VREF"})
	if err == nil {
		t.Fatal("expected a timeout error from the stalled backend")
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("resolve took %s — per-fetch timeout not honoured", elapsed)
	}
}

func mustSet(t *testing.T, s *store.Store, cipher *crypto.Cipher, in store.SecretSet) {
	t.Helper()
	if _, err := s.SetSecret(context.Background(), cipher, in); err != nil {
		t.Fatalf("set %q: %v", in.Name, err)
	}
}
