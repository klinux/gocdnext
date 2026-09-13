package badges

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const (
	TokenBytes         = 32
	TokenEncodedLength = 43
)

// NewToken returns a 256-bit random token encoded for URLs without padding.
func NewToken() (string, error) {
	var raw [TokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// HashToken returns the value persisted in projects.badge_token_hash.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LooksLikeToken is a cheap public-edge filter. It avoids spending a DB lookup
// on obvious junk while still treating every miss as the same unknown badge.
func LooksLikeToken(token string) bool {
	if len(token) != TokenEncodedLength {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
