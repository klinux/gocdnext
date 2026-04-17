// Package github implements GitHub webhook payload handling:
// HMAC SHA-256 signature verification (per X-Hub-Signature-256) and
// push event parsing.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Signature-related errors. Wrap with %w when returning from higher layers.
var (
	ErrEmptySecret            = errors.New("github webhook: empty secret")
	ErrMissingSignature       = errors.New("github webhook: missing signature header")
	ErrInvalidSignatureFormat = errors.New("github webhook: invalid signature format")
	ErrSignatureMismatch      = errors.New("github webhook: signature mismatch")
)

const signaturePrefix = "sha256="

// VerifySignature checks that header matches HMAC-SHA256(secret, body), using
// the format GitHub sends in X-Hub-Signature-256 ("sha256=<hex>").
// Uses constant-time comparison.
func VerifySignature(secret string, body []byte, header string) error {
	if secret == "" {
		return ErrEmptySecret
	}
	if header == "" {
		return ErrMissingSignature
	}
	if !strings.HasPrefix(header, signaturePrefix) {
		return ErrInvalidSignatureFormat
	}

	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return ErrInvalidSignatureFormat
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	if !hmac.Equal(got, want) {
		return ErrSignatureMismatch
	}
	return nil
}
