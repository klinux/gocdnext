package external

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testCAPEM mints a throwaway self-signed CA so the CACert path exercises a
// real PEM (ConfigureTLS parses it — a fake "abc" cert would be rejected).
func testCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gocdnext-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func transportTLS(t *testing.T, addr string, cfg VaultConfig) (*http.Transport, error) {
	t.Helper()
	cfg.Addr = addr
	vc, err := vaultClientConfig(cfg)
	if err != nil {
		return nil, err
	}
	tr, ok := vc.HttpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", vc.HttpClient.Transport)
	}
	return tr, nil
}

func TestVaultAuthenticate_TrimsCredentials(t *testing.T) {
	const addr = "https://vault.example.com"

	t.Run("token auth trims surrounding whitespace", func(t *testing.T) {
		// A token pasted from a terminal / mounted from a k8s Secret often has a
		// trailing newline; Vault would reject it. Token auth is offline (no
		// network), so we can assert the client received the trimmed value.
		b, err := NewVaultBackend(context.Background(), VaultConfig{
			Addr: addr, AuthMethod: VaultAuthToken, Token: "  hvs.abc123\n",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := b.client.Token(); got != "hvs.abc123" {
			t.Fatalf("token = %q, want trimmed %q", got, "hvs.abc123")
		}
	})

	t.Run("approle trims before the empty check", func(t *testing.T) {
		// Whitespace-only role_id/secret_id must trim to empty and be rejected
		// before any login attempt — proves the trim runs (and stays offline).
		_, err := NewVaultBackend(context.Background(), VaultConfig{
			Addr: addr, AuthMethod: VaultAuthAppRole, RoleID: "  ", SecretID: "  \n",
		})
		if err == nil || !strings.Contains(err.Error(), "needs role_id and secret_id") {
			t.Fatalf("err = %v, want approle needs-creds (proves trim before login)", err)
		}
	})
}

func TestVaultClientConfigTLS(t *testing.T) {
	const addr = "https://vault.example.com"
	validCA := testCAPEM(t)

	t.Run("insecure skips verification", func(t *testing.T) {
		tr, err := transportTLS(t, addr, VaultConfig{Insecure: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("InsecureSkipVerify = %v, want true", tr.TLSClientConfig)
		}
	})

	t.Run("ca cert loads a root pool and keeps verification on", func(t *testing.T) {
		tr, err := transportTLS(t, addr, VaultConfig{CACert: validCA})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
			t.Fatal("RootCAs not set from ca_cert")
		}
		if tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("InsecureSkipVerify must stay false when only a CA is given")
		}
	})

	t.Run("invalid ca pem fails loud", func(t *testing.T) {
		_, err := transportTLS(t, addr, VaultConfig{CACert: "not a valid pem"})
		if err == nil {
			t.Fatal("expected an error for an unparseable ca_cert, got nil")
		}
	})

	t.Run("no tls config leaves defaults untouched", func(t *testing.T) {
		tr, err := transportTLS(t, addr, VaultConfig{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// ConfigureTLS isn't invoked, so verification is on (skip stays false).
		if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("InsecureSkipVerify must default to false")
		}
	})
}
