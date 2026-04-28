package apitoken

import (
	"strings"
	"testing"
)

func TestNewUser_RoundTrip(t *testing.T) {
	g, err := NewUser()
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	if !strings.HasPrefix(g.Plaintext, UserPrefix) {
		t.Errorf("plaintext %q missing user prefix", g.Plaintext)
	}
	if strings.HasPrefix(g.Plaintext, SAPrefix) {
		t.Errorf("plaintext %q matches SA prefix — would mis-route", g.Plaintext)
	}
	if len(g.Hash) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(g.Hash))
	}
	if len(g.Prefix) != PrefixDisplayChars {
		t.Errorf("prefix length = %d, want %d", len(g.Prefix), PrefixDisplayChars)
	}
	if g.Kind != KindUser {
		t.Errorf("Kind = %d, want KindUser", g.Kind)
	}

	kind, body, err := Parse(g.Plaintext)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if kind != KindUser {
		t.Errorf("parsed kind = %d, want KindUser", kind)
	}
	if Hash(body) != g.Hash {
		t.Errorf("Hash(body) does not match generated hash")
	}
}

func TestNewSA_RoundTrip(t *testing.T) {
	g, err := NewSA()
	if err != nil {
		t.Fatalf("NewSA: %v", err)
	}
	if !strings.HasPrefix(g.Plaintext, SAPrefix) {
		t.Errorf("plaintext %q missing SA prefix", g.Plaintext)
	}
	kind, body, err := Parse(g.Plaintext)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if kind != KindSA {
		t.Errorf("parsed kind = %d, want KindSA", kind)
	}
	if Hash(body) != g.Hash {
		t.Errorf("Hash(body) does not match generated hash")
	}
}

// TestParse_PrefixOrderingMatters guards the bit of Parse that
// checks SAPrefix BEFORE UserPrefix — UserPrefix is a substring,
// so a naive ordering would mis-classify SA tokens as user tokens.
func TestParse_PrefixOrderingMatters(t *testing.T) {
	g, err := NewSA()
	if err != nil {
		t.Fatalf("NewSA: %v", err)
	}
	kind, _, err := Parse(g.Plaintext)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if kind != KindSA {
		t.Errorf("SA token misclassified as %d (should be KindSA=%d)", kind, KindSA)
	}
}

func TestParse_UnknownPrefix(t *testing.T) {
	_, _, err := Parse("notapis_token")
	if err == nil {
		t.Errorf("Parse should reject non-prefixed bearer")
	}
	_, _, err = Parse("")
	if err == nil {
		t.Errorf("Parse should reject empty bearer")
	}
}

func TestHash_Deterministic(t *testing.T) {
	body := "abcdefghij"
	if Hash(body) != Hash(body) {
		t.Errorf("Hash should be deterministic across calls")
	}
}

func TestPrefixOf(t *testing.T) {
	if got := PrefixOf("abcdef"); got != "abcdef" {
		t.Errorf("short body lost: got %q", got)
	}
	if got := PrefixOf("abcdefghij"); got != "abcdefgh" {
		t.Errorf("long body trimmed wrong: got %q", got)
	}
}
