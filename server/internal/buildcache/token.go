package buildcache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Minted (per-run) build-cache tokens let the scheduler auto-wire a build
// without ever handling the project's static token (only its hash is stored).
// A token is a short-lived, stateless bearer credential:
//
//	bc1.<base64url(projectID[16] || expUnixSec[8])>.<base64url(HMAC-SHA256)>
//
// The endpoint verifies the HMAC with the server signing key (no DB lookup on
// the hot path), checks expiry, and confirms the embedded project id matches
// the project the request path (slug) resolves to — so a token minted for one
// project can't be replayed against another. The "bc1." prefix makes it cheap
// to tell a minted token from a static project token (43-char base64url).
const (
	mintedPrefix  = "bc1."
	payloadLen    = 16 + 8 // projectID + expUnix
	mintSigLen    = 32     // full HMAC-SHA256
	mintedTTLSkew = 30 * time.Second
)

// MintToken produces a per-run token for projectID valid for ttl. key is the
// server build-cache signing secret; an empty key disables minting (returns "").
func MintToken(projectID uuid.UUID, ttl time.Duration, key []byte) string {
	if len(key) == 0 || ttl <= 0 {
		return ""
	}
	var payload [payloadLen]byte
	copy(payload[:16], projectID[:])
	binary.BigEndian.PutUint64(payload[16:], uint64(time.Now().Add(ttl).Unix()))
	sig := mintSign(key, payload[:])
	return mintedPrefix +
		base64.RawURLEncoding.EncodeToString(payload[:]) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyMintedToken checks a token's signature + expiry and returns the project
// id it was minted for. ok=false for any malformed/forged/expired token.
func VerifyMintedToken(token string, key []byte, now time.Time) (uuid.UUID, bool) {
	if len(key) == 0 || !strings.HasPrefix(token, mintedPrefix) {
		return uuid.Nil, false
	}
	parts := strings.Split(token[len(mintedPrefix):], ".")
	if len(parts) != 2 {
		return uuid.Nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) != payloadLen {
		return uuid.Nil, false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(gotSig) != mintSigLen {
		return uuid.Nil, false
	}
	wantSig := mintSign(key, payload)
	if !hmac.Equal(gotSig, wantSig) {
		return uuid.Nil, false
	}
	exp := int64(binary.BigEndian.Uint64(payload[16:]))
	if now.Add(-mintedTTLSkew).Unix() > exp {
		return uuid.Nil, false
	}
	var id uuid.UUID
	copy(id[:], payload[:16])
	return id, true
}

// LooksLikeMinted reports whether a credential is a minted token (vs a static
// project token), so the handler routes it to HMAC verification.
func LooksLikeMinted(token string) bool {
	return strings.HasPrefix(token, mintedPrefix)
}

func mintSign(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	// Domain-separate from any other HMAC use of the same key.
	mac.Write([]byte("gocdnext-buildcache\x00"))
	mac.Write(payload)
	return mac.Sum(nil)
}
