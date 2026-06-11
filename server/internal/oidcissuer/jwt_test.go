package oidcissuer

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return key
}

// TestSignRS256_VerifiesWithStdlib — the signature must verify with
// plain crypto/rsa, independent of any of our own code. This is the
// ground-truth check on the hand-rolled signer.
func TestSignRS256_VerifiesWithStdlib(t *testing.T) {
	key := testRSAKey(t)
	token, err := signRS256(key, "test-kid", map[string]any{"sub": "x", "iss": "y"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("stdlib verify failed: %v", err)
	}
}

// TestSignRS256_HeaderShape — header must be exactly
// {"alg":"RS256","kid":"...","typ":"JWT"}: kid is how verifiers pick
// the key from the JWKS during rotation; a missing kid breaks GCP
// WIF the moment a second key exists.
func TestSignRS256_HeaderShape(t *testing.T) {
	key := testRSAKey(t)
	token, err := signRS256(key, "the-kid", map[string]any{"a": "b"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	headRaw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var head map[string]string
	if err := json.Unmarshal(headRaw, &head); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	want := map[string]string{"alg": "RS256", "kid": "the-kid", "typ": "JWT"}
	if len(head) != len(want) {
		t.Fatalf("header = %v, want exactly %v", head, want)
	}
	for k, v := range want {
		if head[k] != v {
			t.Errorf("header[%s] = %q, want %q", k, head[k], v)
		}
	}
}

// TestSignRS256_NilKey — defensive: a nil key is a programming error
// upstream (issuer constructed without EnsureActiveOIDCKey); fail
// loud, not panic.
func TestSignRS256_NilKey(t *testing.T) {
	if _, err := signRS256(nil, "kid", map[string]any{}); err == nil {
		t.Fatalf("expected error on nil key")
	}
}
