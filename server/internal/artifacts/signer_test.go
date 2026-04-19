package artifacts

import (
	"strings"
	"testing"
	"time"
)

func mustSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

func TestSigner_RoundTrip(t *testing.T) {
	s := mustSigner(t)
	now := time.Now()
	tok := s.Sign("obj/abc123", VerbPUT, now.Add(5*time.Minute))

	key, err := s.Verify(tok, VerbPUT, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if key != "obj/abc123" {
		t.Errorf("key roundtrip: got %q", key)
	}
}

func TestSigner_Verify_Rejects(t *testing.T) {
	s := mustSigner(t)
	now := time.Now()

	tests := []struct {
		name string
		tok  func() string
		at   time.Time
		verb Verb
	}{
		{
			name: "expired",
			tok:  func() string { return s.Sign("k", VerbPUT, now.Add(-time.Second)) },
			at:   now,
			verb: VerbPUT,
		},
		{
			name: "wrong verb",
			tok:  func() string { return s.Sign("k", VerbPUT, now.Add(time.Minute)) },
			at:   now,
			verb: VerbGET,
		},
		{
			name: "tampered sig",
			tok: func() string {
				t := s.Sign("k", VerbPUT, now.Add(time.Minute))
				// flip the last char
				if len(t) > 0 {
					b := []byte(t)
					if b[len(b)-1] == 'A' {
						b[len(b)-1] = 'B'
					} else {
						b[len(b)-1] = 'A'
					}
					return string(b)
				}
				return t
			},
			at:   now,
			verb: VerbPUT,
		},
		{
			name: "wrong signer",
			tok: func() string {
				other, _ := NewSigner([]byte("XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"))
				return other.Sign("k", VerbPUT, now.Add(time.Minute))
			},
			at:   now,
			verb: VerbPUT,
		},
		{
			name: "malformed",
			tok:  func() string { return "not.a.token" },
			at:   now,
			verb: VerbPUT,
		},
		{
			name: "empty",
			tok:  func() string { return "" },
			at:   now,
			verb: VerbPUT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Verify(tt.tok(), tt.verb, tt.at); err == nil {
				t.Errorf("expected ErrBadToken")
			}
		})
	}
}

func TestSigner_Sign_KeysWithDots(t *testing.T) {
	// Storage keys might later embed '.' — the encoding must survive.
	s := mustSigner(t)
	now := time.Now()
	key := "run/abc.def/job/one.tar.gz"
	tok := s.Sign(key, VerbGET, now.Add(time.Minute))
	if strings.Count(tok, ".") != 3 {
		t.Fatalf("token must have exactly 3 dots, got %q", tok)
	}
	got, err := s.Verify(tok, VerbGET, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != key {
		t.Errorf("want %q, got %q", key, got)
	}
}

func TestNewSigner_ShortKeyRejected(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Error("expected short-key error")
	}
}
