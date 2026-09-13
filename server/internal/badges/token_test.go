package badges

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestNewTokenShapeAndEntropy(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken a: %v", err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken b: %v", err)
	}
	if len(a) != TokenEncodedLength {
		t.Fatalf("len(token) = %d, want %d", len(a), TokenEncodedLength)
	}
	if !LooksLikeToken(a) {
		t.Fatalf("generated token rejected by LooksLikeToken: %q", a)
	}
	if a == b {
		t.Fatalf("two generated tokens matched: %q", a)
	}
	if strings.ContainsAny(a, "+/=") {
		t.Fatalf("token is not raw base64url-safe: %q", a)
	}
}

func TestHashTokenShapeAndDeterminism(t *testing.T) {
	token := strings.Repeat("a", TokenEncodedLength)
	got := HashToken(token)
	if len(got) != 64 {
		t.Fatalf("len(hash) = %d, want 64", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("hash is not hex: %q (%v)", got, err)
	}
	if again := HashToken(token); again != got {
		t.Fatalf("HashToken not deterministic: %q vs %q", got, again)
	}
	if other := HashToken(strings.Repeat("b", TokenEncodedLength)); other == got {
		t.Fatalf("different token produced same hash: %q", got)
	}
}

func TestLooksLikeToken(t *testing.T) {
	valid := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq"
	if len(valid) != TokenEncodedLength {
		t.Fatalf("fixture length = %d, want %d", len(valid), TokenEncodedLength)
	}
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "valid alphabet", token: valid, want: true},
		{name: "valid url chars", token: strings.Repeat("-", TokenEncodedLength-1) + "_", want: true},
		{name: "too short", token: strings.Repeat("a", TokenEncodedLength-1), want: false},
		{name: "too long", token: strings.Repeat("a", TokenEncodedLength+1), want: false},
		{name: "padding rejected", token: strings.Repeat("a", TokenEncodedLength-1) + "=", want: false},
		{name: "slash rejected", token: strings.Repeat("a", TokenEncodedLength-1) + "/", want: false},
		{name: "plus rejected", token: strings.Repeat("a", TokenEncodedLength-1) + "+", want: false},
		{name: "space rejected", token: strings.Repeat("a", TokenEncodedLength-1) + " ", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeToken(tc.token); got != tc.want {
				t.Fatalf("LooksLikeToken(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}
