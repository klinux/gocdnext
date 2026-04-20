package authapi

import (
	"crypto/rand"
	"encoding/hex"
)

// randomNonce returns a hex-encoded 16-byte random token used for
// the OIDC `nonce` param. Separate from session / state tokens so
// a leak of one scheme doesn't weaken another.
func randomNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
