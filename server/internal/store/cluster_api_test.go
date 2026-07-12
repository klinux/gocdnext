package store

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// caPEMof re-encodes an httptest TLS server's leaf cert as PEM so the client can
// verify it (instead of falling back to insecure), exercising the real CA path.
func caPEMof(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestDoClusterAPIGet(t *testing.T) {
	t.Run("200 returns the body; path + bearer applied", func(t *testing.T) {
		var gotPath, gotAuth string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"status":{"sync":{"status":"Synced"}}}`))
		}))
		defer srv.Close()

		ep := kubeEndpoint{server: srv.URL, bearer: "tok123", caPEM: caPEMof(t, srv)}
		body, err := doClusterAPIGet(context.Background(), ep, "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/checkout")
		if err != nil {
			t.Fatalf("doClusterAPIGet: %v", err)
		}
		if want := `{"status":{"sync":{"status":"Synced"}}}`; string(body) != want {
			t.Errorf("body = %q, want %q", body, want)
		}
		if gotPath != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/checkout" {
			t.Errorf("server saw path %q", gotPath)
		}
		if gotAuth != "Bearer tok123" {
			t.Errorf("server saw auth %q, want Bearer tok123", gotAuth)
		}
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()
		ep := kubeEndpoint{server: srv.URL, bearer: "t", caPEM: caPEMof(t, srv)}
		_, err := doClusterAPIGet(context.Background(), ep, "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/missing")
		var se *ClusterAPIStatusError
		if !errors.As(err, &se) {
			t.Fatalf("err = %v, want a *ClusterAPIStatusError", err)
		}
		if se.Status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", se.Status)
		}
	})

	t.Run("redirect is refused, not followed with the credential", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, "https://evil.example/steal", http.StatusFound)
		}))
		defer srv.Close()
		ep := kubeEndpoint{server: srv.URL, bearer: "t", caPEM: caPEMof(t, srv)}
		if _, err := doClusterAPIGet(context.Background(), ep, "/x"); err == nil {
			t.Fatal("expected an error on a 3xx (must not follow with a credential), got nil")
		}
	})

	t.Run("http:// endpoint is rejected before any request", func(t *testing.T) {
		ep := kubeEndpoint{server: "http://insecure.example", bearer: "t"}
		if _, err := doClusterAPIGet(context.Background(), ep, "/x"); err == nil {
			t.Fatal("expected an error for a non-HTTPS api_server, got nil")
		}
	})
}
