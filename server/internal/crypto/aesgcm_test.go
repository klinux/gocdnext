package crypto_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
)

func newCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32)) // 32 bytes
	if err != nil {
		t.Fatalf("NewCipherFromHex: %v", err)
	}
	return c
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c := newCipher(t)
	plain := []byte("ghp_supersecret_value_123")

	blob, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip lost data: got %q, want %q", got, plain)
	}
}

func TestEncrypt_EmptyPlaintext(t *testing.T) {
	c := newCipher(t)
	blob, err := c.Encrypt([]byte{})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty roundtrip got %d bytes", len(got))
	}
}

func TestEncrypt_DifferentCiphertextEachTime(t *testing.T) {
	c := newCipher(t)
	plain := []byte("same value")

	a, _ := c.Encrypt(plain)
	b, _ := c.Encrypt(plain)
	if bytes.Equal(a, b) {
		t.Fatalf("ciphertexts identical — nonce randomness broken")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	c1, _ := crypto.NewCipherFromHex(strings.Repeat("11", 32))
	c2, _ := crypto.NewCipherFromHex(strings.Repeat("22", 32))

	blob, _ := c1.Encrypt([]byte("top secret"))
	if _, err := c2.Decrypt(blob); err == nil {
		t.Fatalf("decrypt with wrong key succeeded")
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	c := newCipher(t)
	blob, _ := c.Encrypt([]byte("x"))
	// Flip a byte in the ciphertext region (after the 12-byte nonce).
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatalf("decrypt of tampered blob succeeded")
	}
}

func TestDecrypt_TruncatedBlobFails(t *testing.T) {
	c := newCipher(t)
	if _, err := c.Decrypt([]byte{1, 2, 3}); err == nil {
		t.Fatalf("decrypt of short blob succeeded")
	}
}

func TestNewCipherFromHex_RejectsShortKey(t *testing.T) {
	if _, err := crypto.NewCipherFromHex("deadbeef"); err == nil {
		t.Fatalf("accepted short key")
	}
}

func TestNewCipherFromHex_RejectsInvalidHex(t *testing.T) {
	if _, err := crypto.NewCipherFromHex("not-hex-at-all-and-the-length-is-wrong"); err == nil {
		t.Fatalf("accepted non-hex key")
	}
}

func TestCipher_BlobStartsWithNonce(t *testing.T) {
	// Sanity check on the framing: nonce is 12 bytes at the start.
	c := newCipher(t)
	blob, _ := c.Encrypt([]byte("hello"))
	if len(blob) < 12+1 { // nonce + at least 1 byte of GCM tag+ct
		t.Fatalf("blob too short: %d", len(blob))
	}
	// First 12 bytes should be the nonce — not a zero vector (randomness).
	if bytes.Equal(blob[:12], make([]byte, 12)) {
		t.Fatalf("nonce appears to be zero")
	}
	_ = hex.EncodeToString // keep import if Go balks
}
