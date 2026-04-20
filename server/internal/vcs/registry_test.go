package vcs_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

func testPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

func newAppClient(t *testing.T) *ghscm.AppClient {
	t.Helper()
	c, err := ghscm.NewAppClient(ghscm.AppConfig{
		AppID:         1,
		PrivateKeyPEM: testPEM(t),
	})
	if err != nil {
		t.Fatalf("app client: %v", err)
	}
	return c
}

func TestRegistry_ReplaceIsAtomic(t *testing.T) {
	r := vcs.New()
	if r.GitHubApp() != nil {
		t.Fatalf("fresh registry must return nil GitHubApp")
	}
	if r.Len() != 0 {
		t.Fatalf("fresh registry len = %d", r.Len())
	}

	app := newAppClient(t)
	r.Replace(app, []vcs.Integration{{
		Name: "primary", Kind: "github_app", Enabled: true, Source: vcs.SourceEnv,
	}})
	if r.GitHubApp() != app {
		t.Fatalf("GitHubApp should return the app we replaced in")
	}
	if r.Len() != 1 {
		t.Fatalf("len = %d after replace", r.Len())
	}

	// Second Replace must fully swap the slice; the old entry is
	// gone.
	r.Replace(nil, []vcs.Integration{{
		Name: "secondary", Kind: "github_app", Enabled: false, Source: vcs.SourceDB,
	}})
	if r.GitHubApp() != nil {
		t.Fatalf("GitHubApp should be nil after replace(nil, …)")
	}
	list := r.List()
	if len(list) != 1 || list[0].Name != "secondary" {
		t.Fatalf("old entry leaked: %+v", list)
	}
}

func TestRegistry_ListReturnsCopy(t *testing.T) {
	r := vcs.New()
	r.Replace(nil, []vcs.Integration{{Name: "a", Kind: "github_app"}})
	list := r.List()
	list[0].Name = "mutated-outside"
	// The internal store keeps the original value.
	if r.List()[0].Name != "a" {
		t.Fatalf("List() returned a live reference to internal state")
	}
}
