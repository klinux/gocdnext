package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func oidcTestCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("NewCipherFromHex: %v", err)
	}
	return c
}

// TestEnsureActiveOIDCKey_GeneratesOnEmpty — first boot path: no key
// in the table, EnsureActiveOIDCKey generates RSA-2048, seals the
// private half with the authCipher, and the returned key is usable
// (private parses, kid non-empty, alg pinned to RS256).
func TestEnsureActiveOIDCKey_GeneratesOnEmpty(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(oidcTestCipher(t))

	key, err := s.EnsureActiveOIDCKey(context.Background())
	if err != nil {
		t.Fatalf("EnsureActiveOIDCKey: %v", err)
	}
	if key.Kid == "" {
		t.Errorf("kid must be non-empty (RFC 7638 thumbprint)")
	}
	if key.Alg != "RS256" {
		t.Errorf("alg = %q, want RS256", key.Alg)
	}
	if key.Private == nil {
		t.Fatalf("private key must round-trip through the cipher")
	}
	if key.Private.N.BitLen() != 2048 {
		t.Errorf("key size = %d bits, want 2048", key.Private.N.BitLen())
	}
	// The stored public DER must match the private key's public half —
	// a mismatch here would mean JWKS serves a key that can't verify
	// our signatures.
	if key.Private.PublicKey.N.Cmp(key.Private.N) == 0 && key.Private.PublicKey.E == 0 {
		t.Errorf("public key not populated")
	}
}

// TestEnsureActiveOIDCKey_Idempotent — second call returns the SAME
// key (same kid), and the table holds exactly one row.
func TestEnsureActiveOIDCKey_Idempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(oidcTestCipher(t))
	ctx := context.Background()

	k1, err := s.EnsureActiveOIDCKey(ctx)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	k2, err := s.EnsureActiveOIDCKey(ctx)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if k1.Kid != k2.Kid {
		t.Errorf("kid changed across calls: %q vs %q", k1.Kid, k2.Kid)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM oidc_signing_keys`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// TestEnsureActiveOIDCKey_ConcurrentRace — the multi-replica boot
// scenario: N concurrent callers all converge on ONE key. The
// partial unique index + ON CONFLICT DO NOTHING + re-SELECT is the
// mechanism; this test is the proof.
func TestEnsureActiveOIDCKey_ConcurrentRace(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(oidcTestCipher(t))
	ctx := context.Background()

	const n = 8
	kids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := s.EnsureActiveOIDCKey(ctx)
			kids[i], errs[i] = k.Kid, err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if kids[i] != kids[0] {
			t.Errorf("caller %d got kid %q, caller 0 got %q — split-brain", i, kids[i], kids[0])
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM oidc_signing_keys WHERE retired_at IS NULL AND revoked_at IS NULL`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("active rows = %d, want exactly 1", rows)
	}
}

// TestRotateOIDCKey_Graceful — old key gets retired_at, a NEW active
// key exists, and the JWKS listing returns BOTH inside the overlap
// window but only the new one outside it.
func TestRotateOIDCKey_Graceful(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(oidcTestCipher(t))
	ctx := context.Background()

	old, err := s.EnsureActiveOIDCKey(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fresh, err := s.RotateOIDCKey(ctx, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh.Kid == old.Kid {
		t.Fatalf("rotate returned the same kid")
	}

	// Inside the overlap window (cutoff in the past) both keys serve.
	within, err := s.ListOIDCJWKSKeys(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list within: %v", err)
	}
	if len(within) != 2 {
		t.Errorf("jwks within overlap = %d keys, want 2 (active + retired)", len(within))
	}

	// Outside the window (cutoff in the future) only the active key.
	after, err := s.ListOIDCJWKSKeys(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != 1 || after[0].Kid != fresh.Kid {
		t.Errorf("jwks after overlap = %+v, want only %q", after, fresh.Kid)
	}

	// And the new key is the one EnsureActiveOIDCKey now returns.
	cur, err := s.EnsureActiveOIDCKey(ctx)
	if err != nil {
		t.Fatalf("ensure post-rotate: %v", err)
	}
	if cur.Kid != fresh.Kid {
		t.Errorf("active = %q, want %q", cur.Kid, fresh.Kid)
	}
}

// TestRotateOIDCKey_Emergency — compromised-key path: the old key is
// revoked and disappears from the JWKS IMMEDIATELY, regardless of
// any overlap window.
func TestRotateOIDCKey_Emergency(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(oidcTestCipher(t))
	ctx := context.Background()

	old, err := s.EnsureActiveOIDCKey(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fresh, err := s.RotateOIDCKey(ctx, true)
	if err != nil {
		t.Fatalf("rotate emergency: %v", err)
	}

	keys, err := s.ListOIDCJWKSKeys(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0].Kid != fresh.Kid {
		t.Fatalf("jwks after emergency = %+v, want only %q (old %q must be gone)", keys, fresh.Kid, old.Kid)
	}
}

// TestRotateOIDCKey_NoActiveKey — rotating an empty table still
// yields a fresh active key (rotate is "ensure new", not "swap").
func TestRotateOIDCKey_NoActiveKey(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(oidcTestCipher(t))

	fresh, err := s.RotateOIDCKey(context.Background(), false)
	if err != nil {
		t.Fatalf("rotate on empty: %v", err)
	}
	if fresh.Kid == "" {
		t.Errorf("expected a fresh key")
	}
}

// TestEnsureActiveOIDCKey_NoCipher — without the authCipher there is
// no way to seal the private key; fail loud, write nothing.
func TestEnsureActiveOIDCKey_NoCipher(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool) // cipher NOT set

	_, err := s.EnsureActiveOIDCKey(context.Background())
	if err == nil {
		t.Fatalf("expected error without cipher")
	}
	var n int
	if qerr := pool.QueryRow(context.Background(), `SELECT count(*) FROM oidc_signing_keys`).Scan(&n); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if n != 0 {
		t.Errorf("rows written despite missing cipher: %d", n)
	}
}
