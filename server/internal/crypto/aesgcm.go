// Package crypto wraps AES-256-GCM for at-rest encryption of secret values.
// The whole surface is deliberately small — one Cipher type with Encrypt and
// Decrypt; nothing else. Framing is `nonce||ciphertext-with-tag`, which the
// stdlib AEAD already produces via Seal(nonce, nonce, ...).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Cipher holds the AEAD derived from a 32-byte key. Construct once at boot
// and share — the underlying type is safe for concurrent use.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher takes a raw 32-byte key and returns a ready-to-use Cipher.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// NewCipherFromHex decodes a hex-encoded 64-character key (32 bytes).
func NewCipherFromHex(s string) (*Cipher, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode hex key: %w", err)
	}
	return NewCipher(b)
}

// Encrypt seals plaintext with a fresh random nonce. Output layout is
// nonce (12 bytes) || ciphertext || GCM auth tag (16 bytes).
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: read nonce: %w", err)
	}
	// Seal appends the ciphertext+tag to nonce and returns the combined slice.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt. Returns an error on truncated input, wrong key,
// or tampered ciphertext (AEAD auth-tag mismatch).
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns+c.aead.Overhead() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plain, nil
}
