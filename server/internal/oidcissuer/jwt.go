package oidcissuer

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// signRS256 produces a compact JWT: base64url(header).base64url(
// payload).base64url(signature), RS256 (RSASSA-PKCS1-v1_5 + SHA-256).
// Same construction as scm/github/app.go mintJWT, generalized with a
// kid header — verifiers use kid to pick the right key out of the
// JWKS during rotation; without it GCP WIF breaks the moment a
// second key exists.
//
// We only SIGN here. gocdnext never parses or verifies untrusted
// JWTs in this feature, which is why no JWT library is imported:
// the historical CVE surface (alg confusion, `none`, key-type
// confusion) lives entirely on the verification side.
func signRS256(key *rsa.PrivateKey, kid string, payload map[string]any) (string, error) {
	if key == nil {
		return "", fmt.Errorf("oidcissuer: sign: nil private key")
	}
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("oidcissuer: marshal header: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("oidcissuer: marshal payload: %w", err)
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(body)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("oidcissuer: sign: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}
