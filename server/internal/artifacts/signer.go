package artifacts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signer mints and verifies short-lived capability tokens used by the
// filesystem backend. A token encodes storage_key, HTTP verb (PUT or GET),
// and expiry; the HMAC signature binds them. Server side trusts the
// token if HMAC matches and exp is in the future.
//
// Format (URL-safe base64, "." separator):
//
//	<storage_key>.<verb>.<exp_unix>.<sig>
//
// `sig` is HMAC-SHA256(secret, "<storage_key>.<verb>.<exp_unix>"), then
// base64 no-padding. Simpler than JWT/PASETO; same threat model
// (tamper = HMAC fails, replay = bounded by TTL).
type Signer struct {
	secret []byte
}

// Verb is the HTTP method the token authorises.
type Verb string

const (
	VerbPUT Verb = "PUT"
	VerbGET Verb = "GET"
)

// ErrBadToken is what Verify returns for any tampering / expiry / format
// issue. Handlers return 401 — never leak which part failed.
var ErrBadToken = errors.New("artifacts: invalid or expired token")

// NewSigner wraps a secret key. Key ≥ 32 bytes strongly recommended; we
// require ≥ 16 to avoid the accidental empty-key footgun.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < 16 {
		return nil, errors.New("artifacts: signer secret must be >= 16 bytes")
	}
	key := make([]byte, len(secret))
	copy(key, secret)
	return &Signer{secret: key}, nil
}

// Sign mints a token for (key, verb) good until expiresAt.
func (s *Signer) Sign(key string, verb Verb, expiresAt time.Time) string {
	payload := tokenPayload(key, verb, expiresAt)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

// Verify returns the storage_key if the token is well-formed, signed by
// this Signer, authorises the given verb, and is still valid. Any issue
// collapses to ErrBadToken — callers must not leak specifics.
func (s *Signer) Verify(token string, want Verb, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return "", ErrBadToken
	}
	keyB64, verb, expStr, sig := parts[0], parts[1], parts[2], parts[3]

	if Verb(verb) != want {
		return "", ErrBadToken
	}

	expUnix, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", ErrBadToken
	}
	exp := time.Unix(expUnix, 0)
	if now.After(exp) {
		return "", ErrBadToken
	}

	payload := keyB64 + "." + verb + "." + expStr
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	want2 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want2)) {
		return "", ErrBadToken
	}

	raw, err := base64.RawURLEncoding.DecodeString(keyB64)
	if err != nil {
		return "", ErrBadToken
	}
	return string(raw), nil
}

// tokenPayload builds "<key_b64>.<verb>.<exp_unix>". The key is
// base64-encoded so arbitrary bytes don't collide with the '.' separator.
func tokenPayload(key string, verb Verb, exp time.Time) string {
	kb := base64.RawURLEncoding.EncodeToString([]byte(key))
	return fmt.Sprintf("%s.%s.%d", kb, verb, exp.Unix())
}
