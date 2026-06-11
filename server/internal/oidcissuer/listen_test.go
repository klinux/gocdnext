package oidcissuer_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/oidcissuer"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

type countingInvalidator struct{ n atomic.Int64 }

func (c *countingInvalidator) HandleRotationNotice(string) { c.n.Add(1) }

// TestListenForRotations_EndToEnd — the cross-replica contract on a
// REAL Postgres: a listener on one "replica" hears the NOTIFY fired
// inside another replica's rotation transaction and receives the
// rotation notice (the real issuer converges idempotently by kid;
// the fake here just counts deliveries). This is what shrinks
// remote convergence from "within the cache TTL" to ~ms.
func TestListenForRotations_EndToEnd(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetAuthCipher(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.EnsureActiveOIDCKey(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	inv := &countingInvalidator{}
	go oidcissuer.ListenForRotations(ctx, dbtest.DSN(), inv, nil)

	// Listener startup is async — wait for the LISTEN to be
	// established by probing with a first rotation until the
	// notification lands. Bounded loop; flake-resistant without
	// fixed sleeps.
	deadline := time.Now().Add(15 * time.Second)
	for inv.n.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("listener never received a rotation NOTIFY")
		}
		if _, err := s.RotateOIDCKey(ctx, false); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// One more rotation with the listener established must land
	// promptly.
	before := inv.n.Load()
	if _, err := s.RotateOIDCKey(ctx, true); err != nil {
		t.Fatalf("rotate emergency: %v", err)
	}
	landed := false
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		if inv.n.Load() > before {
			landed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !landed {
		t.Fatalf("emergency rotation NOTIFY not received within 5s")
	}
}
