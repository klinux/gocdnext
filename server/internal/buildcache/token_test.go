package buildcache

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	key := []byte("a-signing-key-at-least-32-bytes-long")
	id := uuid.New()
	tok := MintToken(id, time.Hour, key)
	if !LooksLikeMinted(tok) {
		t.Fatalf("minted token %q not recognized", tok)
	}
	got, ok := VerifyMintedToken(tok, key, time.Now())
	if !ok || got != id {
		t.Fatalf("verify = (%s,%v), want (%s,true)", got, ok, id)
	}
}

func TestVerifyRejects(t *testing.T) {
	key := []byte("a-signing-key-at-least-32-bytes-long")
	id := uuid.New()
	tok := MintToken(id, time.Hour, key)

	tests := []struct {
		name  string
		token string
		key   []byte
		now   time.Time
	}{
		{"wrong key", tok, []byte("a-different-key-also-32-bytes-long!!"), time.Now()},
		{"expired", MintToken(id, time.Second, key), key, time.Now().Add(time.Hour)},
		{"tampered sig", tok[:len(tok)-2] + "aa", key, time.Now()},
		{"not minted", "not-a-minted-token", key, time.Now()},
		{"empty key disables", tok, nil, time.Now()},
		{"garbage payload", "bc1.!!!.!!!", key, time.Now()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := VerifyMintedToken(tt.token, tt.key, tt.now); ok {
				t.Errorf("verify accepted %s", tt.name)
			}
		})
	}
}

func TestMintEmptyKeyOrTTL(t *testing.T) {
	if MintToken(uuid.New(), time.Hour, nil) != "" {
		t.Error("empty key should mint nothing")
	}
	if MintToken(uuid.New(), 0, []byte("key-32-bytes-long-for-the-hmac-xx")) != "" {
		t.Error("non-positive ttl should mint nothing")
	}
}
