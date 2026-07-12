package store

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
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

func TestDoClusterAPIWrite(t *testing.T) {
	t.Run("PATCH forwards method, content-type, body; 2xx returns body", func(t *testing.T) {
		var gotMethod, gotCT, gotAuth string
		var gotBody []byte
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotCT, gotAuth = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"metadata":{"name":"checkout"}}`))
		}))
		defer srv.Close()

		ep := kubeEndpoint{server: srv.URL, bearer: "tok123", caPEM: caPEMof(t, srv)}
		body, err := doClusterAPIWrite(context.Background(), ep, http.MethodPatch,
			"application/merge-patch+json", "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/checkout",
			[]byte(`{"operation":{"sync":{}}}`))
		if err != nil {
			t.Fatalf("doClusterAPIWrite: %v", err)
		}
		if gotMethod != http.MethodPatch {
			t.Errorf("method = %q, want PATCH", gotMethod)
		}
		if gotCT != "application/merge-patch+json" {
			t.Errorf("content-type = %q", gotCT)
		}
		if gotAuth != "Bearer tok123" {
			t.Errorf("auth = %q", gotAuth)
		}
		if string(gotBody) != `{"operation":{"sync":{}}}` {
			t.Errorf("server saw body %q", gotBody)
		}
		if string(body) != `{"metadata":{"name":"checkout"}}` {
			t.Errorf("response body = %q", body)
		}
	})

	t.Run("non-2xx is a ClusterAPIStatusError", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "conflict", http.StatusConflict)
		}))
		defer srv.Close()
		ep := kubeEndpoint{server: srv.URL, bearer: "t", caPEM: caPEMof(t, srv)}
		_, err := doClusterAPIWrite(context.Background(), ep, http.MethodPatch,
			"application/merge-patch+json", "/x", []byte(`{}`))
		var se *ClusterAPIStatusError
		if !errors.As(err, &se) || se.Status != http.StatusConflict {
			t.Fatalf("err = %v, want a *ClusterAPIStatusError with 409", err)
		}
	})

	t.Run("http:// endpoint is rejected before any request", func(t *testing.T) {
		ep := kubeEndpoint{server: "http://insecure.example", bearer: "t"}
		if _, err := doClusterAPIWrite(context.Background(), ep, http.MethodPatch, "application/merge-patch+json", "/x", []byte(`{}`)); err == nil {
			t.Fatal("expected an error for a non-HTTPS api_server, got nil")
		}
	})
}
