package buildcache

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var testGen = []byte("deadbeefcafebabe-token-hash-generation")

func TestMintVerifyRoundTrip(t *testing.T) {
	key := []byte("a-signing-key-at-least-32-bytes-long")
	id := uuid.New()
	for _, write := range []bool{true, false} {
		tok := MintToken(id, write, time.Hour, key, testGen)
		if !LooksLikeMinted(tok) {
			t.Fatalf("minted token %q not recognized", tok)
		}
		got, gotWrite, ok := VerifyMintedToken(tok, key, testGen, time.Now())
		if !ok || got != id || gotWrite != write {
			t.Fatalf("verify = (%s,write=%v,%v), want (%s,write=%v,true)", got, gotWrite, ok, id, write)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	key := []byte("a-signing-key-at-least-32-bytes-long")
	id := uuid.New()
	tok := MintToken(id, true, time.Hour, key, testGen)

	tests := []struct {
		name  string
		token string
		key   []byte
		gen   []byte
		now   time.Time
	}{
		{"wrong key", tok, []byte("a-different-key-also-32-bytes-long!!"), testGen, time.Now()},
		{"wrong generation (rotated/re-enabled)", tok, key, []byte("a-different-generation-hash-value!!!"), time.Now()},
		{"empty generation (disabled)", tok, key, nil, time.Now()},
		{"expired", MintToken(id, true, time.Second, key, testGen), key, testGen, time.Now().Add(time.Hour)},
		{"tampered sig", tok[:len(tok)-2] + "aa", key, testGen, time.Now()},
		{"not minted", "not-a-minted-token", key, testGen, time.Now()},
		{"empty key disables", tok, nil, testGen, time.Now()},
		{"garbage payload", "bc1.!!!.!!!", key, testGen, time.Now()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, ok := VerifyMintedToken(tt.token, tt.key, tt.gen, tt.now); ok {
				t.Errorf("verify accepted %s", tt.name)
			}
		})
	}
}

func TestMintEmptyKeyGenOrTTL(t *testing.T) {
	key := []byte("key-32-bytes-long-for-the-hmac-xxx")
	if MintToken(uuid.New(), true, time.Hour, nil, testGen) != "" {
		t.Error("empty key should mint nothing")
	}
	if MintToken(uuid.New(), true, time.Hour, key, nil) != "" {
		t.Error("empty generation should mint nothing")
	}
	if MintToken(uuid.New(), true, 0, key, testGen) != "" {
		t.Error("non-positive ttl should mint nothing")
	}
}
