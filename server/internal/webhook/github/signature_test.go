package github_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	const secret = "it's-a-secret-to-everybody"
	body := []byte(`{"ref":"refs/heads/main"}`)
	validSig := "sha256=" + computeHex(t, secret, body)

	tests := []struct {
		name    string
		secret  string
		body    []byte
		header  string
		wantErr error
	}{
		{
			name:   "valid signature",
			secret: secret,
			body:   body,
			header: validSig,
		},
		{
			name:    "empty header",
			secret:  secret,
			body:    body,
			header:  "",
			wantErr: github.ErrMissingSignature,
		},
		{
			name:    "wrong prefix",
			secret:  secret,
			body:    body,
			header:  "sha1=" + computeHex(t, secret, body),
			wantErr: github.ErrInvalidSignatureFormat,
		},
		{
			name:    "non-hex payload",
			secret:  secret,
			body:    body,
			header:  "sha256=zzzz",
			wantErr: github.ErrInvalidSignatureFormat,
		},
		{
			name:    "signature does not match",
			secret:  secret,
			body:    body,
			header:  "sha256=" + computeHex(t, "wrong-secret", body),
			wantErr: github.ErrSignatureMismatch,
		},
		{
			name:    "tampered body",
			secret:  secret,
			body:    []byte(`{"ref":"refs/heads/evil"}`),
			header:  validSig,
			wantErr: github.ErrSignatureMismatch,
		},
		{
			name:    "empty secret rejected",
			secret:  "",
			body:    body,
			header:  validSig,
			wantErr: github.ErrEmptySecret,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := github.VerifySignature(tt.secret, tt.body, tt.header)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("VerifySignature err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func computeHex(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
